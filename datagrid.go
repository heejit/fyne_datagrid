// Package fynex_datagrid is a self-contained Fyne data-grid widget that is
// built from a Go struct type (see New). Everything lives in this one file and
// there is no dependency on any other grid package.
//
// Section index (search for the title in a "// ----" banner; the order below
// is the order in the file):
//
//	Column / Row definitions   Column, ColType, CellStyle, TextCol/IntCol/NumberCol/DateCol, Row, CellChange
//	Data source                DataSource, EditableSource, func sources
//	DataGrid widget            the DataGrid struct, its exported options and callbacks, NewDataGrid
//	Public API                 Reload / Repaint, SelectIndex, Sum, column and row accessors
//	Row operations             AppendRow, InsertRowAt, DeleteRowAt, TappedSecondary (context menu)
//	Sorting / view             SortBy, rebuildView (view = display order, pos = inverse)
//	Layout metrics, scrolling  layoutMetrics, scrollbars, Scrolled, Dragged (column resize)
//	Mouse                      Tapped, clickCell, selectAt, double-click detection
//	Focus + keyboard           TypedKey, moveTo / moveToExt, Tab and Enter stepping, Alt+Left/Right column resize
//	Editing                    beginEdit, commitEdit, clearCell
//	Renderer                   dataGridRenderer: pooled canvas objects, syncRow, syncCell, cursor
//	Inline editor entry        cellEntry (Enter / Esc / Tab / arrows handed back to the grid)
//	Value helpers              parsing, formatting, sorting comparers, thousand separators
//	Struct-tag binding         New[T]: builds a grid from a struct type via reflection
//	Column option accessors    ColumnByKey, UpdateColumn, Set/Column<Option> pairs
//	Grid option accessors      Is/Set pairs for ReadOnly, Sortable, Striped, Resizable, ...
//	Cell font / color          StyleCell support (styleOf, sameColor)
//	Enter key hook             OnEnter support (fireEnter, ShiftDown)
//	Multi-row selection        rowSet bitset, Ctrl/Shift click and keys, Select* API, DeleteSelectedRows
//	Multi-cell selection       rectangle of cells: Shift+click / Shift+arrows / mouse drag, Ctrl+C, SelectedCellRange
//	Copy / export              context-menu Copy cell / rows (CSV, PSV), ExportCSV, WriteCSV / WritePSV
//	Paste                      context-menu Paste cell(s), Ctrl+V, PasteCells
//
// Typical cross-cutting change: a new option touches the DataGrid struct, the
// relevant accessor section and the renderer (syncRow / syncCell).
package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"image/color"
	"io"
	"math/bits"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/shopspring/decimal"
)

const dateLayout = "02-Jan-2006"

// ---------------------------------------------------------------------------
// Column / Row definitions  (equivalent of MyGrid's cnm / calg / csz / ctp)
// ---------------------------------------------------------------------------

type ColType int

const (
	ColText ColType = iota
	ColNumber
	ColDate
	ColInt // int64 whole numbers
)

type Column struct {
	Key        string         // key in Row (like the 'dr','cr','date' keys in ctp)
	Title      string         // header text
	Type       ColType        // text / number / date / int
	WidthPct   float32        // width as a percent of the grid, e.g. 10 = 10%; unused space stays empty
	Align      fyne.TextAlign // leading / trailing / center
	Decimals   int            // numbers only
	Thousands  bool           // numbers and ints only: show thousand separators (1,234,567.00)
	// Formatter, if set, overrides how a cell value is displayed. The cell
	// func still returns the raw value (decimal.Decimal, int, ...), so sorting
	// and editing keep working on real numbers. It wins over Thousands.
	Formatter func(v any) string
	DateLayout string         // dates only (display + first layout tried when parsing)
	HideZero   bool           // numbers only: show blank instead of 0.00
	ReadOnly   bool           // this column can't be edited even if the grid is editable
	MinWidth   float32        // optional floor in px (0 = none, width is exactly WidthPct)
}

// CellStyle is the look of a cell's text, returned by DataGrid.StyleCell.
// Every field is optional: a zero value means "use the default".
type CellStyle struct {
	Color      color.Color // text color; nil = theme foreground
	Background color.Color // cell background; nil = none (row stripe / selection shows)
	Bold       bool
	Italic     bool
	Monospace  bool
	Size       float32 // text size in px; 0 = theme text size (keep it near the theme size: the row height does not grow)
}

func (s CellStyle) textStyle() fyne.TextStyle {
	return fyne.TextStyle{Bold: s.Bold, Italic: s.Italic, Monospace: s.Monospace}
}

func TextCol(key, title string, pct float32) Column {
	return Column{Key: key, Title: title, Type: ColText, WidthPct: pct}
}

// IntCol is a column of int64 values. Like TextCol it takes only key, title
// and width; values are left-aligned, shown as plain integers, sorted
// numerically and parsed as int64 when edited.
func IntCol(key, title string, pct float32) Column {
	return Column{Key: key, Title: title, Type: ColInt, WidthPct: pct}
}

func NumberCol(key, title string, pct float32) Column {
	return Column{Key: key, Title: title, Type: ColNumber, WidthPct: pct,
		Align: fyne.TextAlignTrailing, Decimals: 2}
}

func DateCol(key, title string, pct float32) Column {
	return Column{Key: key, Title: title, Type: ColDate, WidthPct: pct, DateLayout: dateLayout}
}

// A row is a map keyed by Column.Key. Values: string, int*, decimal.Decimal
// (number columns), int64 (int columns), time.Time.
type Row map[string]any

// CellChange describes one edited cell (the "cell changed" event payload).
type CellChange struct {
	RowIndex int    // index into your slice (stable across sorting)
	Col      int    // column index
	Key      string // column key
	Old, New any
	Row      Row
}

// ---------------------------------------------------------------------------
// Data source: the grid reads its data through this interface, it never
// copies it. Only the rows currently on screen are ever asked for.
// ---------------------------------------------------------------------------

// DataSource supplies the data. row is an index 0..Len()-1; key is Column.Key.
type DataSource interface {
	Len() int
	Value(row int, key string) any
}

// EditableSource is an optional extension: if your source also implements
// SetValue, cells become editable (when the grid's ReadOnly is true).
// Return an error to reject the value; the editor stays open and shows it.
type EditableSource interface {
	DataSource
	SetValue(row int, key string, v any) error
}

type funcSource struct {
	count func() int
	get   func(row int, key string) any
}

func (s *funcSource) Len() int                      { return s.count() }
func (s *funcSource) Value(row int, key string) any { return s.get(row, key) }

type editableFuncSource struct {
	funcSource
	set func(row int, key string, v any) error
}

func (s *editableFuncSource) SetValue(row int, key string, v any) error { return s.set(row, key, v) }

// newFuncSource builds a DataSource from closures. set == nil means read-only.
func newFuncSource(count func() int, get func(row int, key string) any,
	set func(row int, key string, v any) error) DataSource {
	fs := funcSource{count: count, get: get}
	if set == nil {
		return &fs
	}
	return &editableFuncSource{funcSource: fs, set: set}
}

// emptySource is what a grid shows before it has data.
type emptySource struct{}

func (emptySource) Len() int               { return 0 }
func (emptySource) Value(int, string) any { return nil }

// ---------------------------------------------------------------------------
// DataGrid widget
//
// This is a from-scratch widget: it does NOT use widget.Table. Rendering is
// done by dataGridRenderer (below), which keeps a small fixed pool of canvas
// objects (one set per *visible* row) and re-points them at the data on every
// refresh. Cost per refresh is therefore O(visible rows x columns), no matter
// how many rows the grid holds. Scrolling is row-granular, there is no
// per-cell widget, and one single Entry is reused as the inline editor.
// ---------------------------------------------------------------------------

// scrollRowsPerNotch: rows moved per mouse-wheel notch (Fyne reports ~10 units
// of Scrolled.DY per notch). Touchpads report smaller fractional values, which
// are accumulated.
const scrollRowsPerNotch = 3

// scrollPxPerNotch: pixels moved per mouse-wheel notch when scrolling sideways.
const scrollPxPerNotch = 60

const (
	edgeGrab      float32 = 5  // px either side of a header edge that grabs it
	minUserColW   float32 = 24 // narrowest a user can drag a column
	keyResizeStep      float32 = 10 // px per Alt+Left / Alt+Right press
	keyResizeStepShift float32 = 50 // px per press with Shift also held
)

type dragMode int

const (
	dragNone dragMode = iota
	dragBar           // dragging the scrollbar thumb
	dragHBar          // dragging the horizontal scrollbar thumb
	dragCol           // resizing a column by its header edge
	dragOther         // a drag that started on a body cell (selects cells) or elsewhere (ignored)
	dragContent       // touch: the finger drags the rows / columns (mobile only)
)

type editState struct {
	row  int // source row index
	col  int
	old  any
	text string
}

type DataGrid struct {
	widget.BaseWidget

	cols    []Column
	colsGen uint32 // bumped by SetColumns so the renderer rebuilds its pools
	src     DataSource
	view    []int // display order: view[displayPos] = source index

	lengthFn  func() int                    // from NewDataGrid (may be nil)
	cellFn    func(row int, key string) any // from NewDataGrid (may be nil)
	viewStale bool                          // view must be (re)built before first layout
	pos  []int                              // inverse: pos[sourceIndex] = displayPos

	selected int // source index of cursor row, -1 = none
	curCol   int // cursor column

	// multi-row selection (see "Multi-row selection" near the end of the file)
	sel    rowSet           // selected rows by source index; only used while selOn
	selOn  bool             // true once rows were toggled / ranged; false = just the cursor row
	anchor int              // source index a Shift range starts from, -1 = none
	mods   fyne.KeyModifier // modifier keys held at the last mouse press

	// multi-cell selection (see "Multi-cell selection" near the end of the file).
	// The rectangle runs from the anchor cell (row g.anchor, column anchorCol)
	// to the cursor cell (g.selected, g.curCol).
	cellOn    bool // a rectangle of more than one cell is selected
	anchorCol int  // column of the anchor cell, -1 = unknown
	dragCells bool // the running mouse drag selects cells
	sortCol  int
	sortAsc  bool

	focused bool
	shift   bool
	edit    *editState
	entry   *cellEntry        // the single inline editor, shown over the cursor cell
	rend    *dataGridRenderer // live renderer (nil until the grid is first shown)
	quiet   bool              // true while we set entry text from code

	// double-click detection (see Tapped)
	lastTap    time.Time
	lastTapRow int
	lastTapCol int

	// scrolling + layout state (maintained by layoutMetrics / the renderer)
	topRow    int     // first visible display row
	scrollAcc float32 // fractional wheel / touchpad scroll, in rows
	drag      dragMode
	textSize  float32
	rowH      float32
	headerH   float32
	visRows   int
	hasBar    bool
	sbW       float32 // scrollbar width (0 when not needed)
	thumbY    float32
	thumbH    float32
	colX      []float32 // screen x of each column (already shifted by leftX)
	colW      []float32

	// horizontal scrolling (used when the columns don't fit their minimum widths)
	leftX    float32 // how far the content is scrolled to the left, in px
	contentW float32 // total width of all columns
	viewW    float32 // visible width (widget width minus the vertical scrollbar)
	hasH     bool
	sbH      float32 // horizontal scrollbar height (0 when not needed)
	hthumbX  float32
	hthumbW  float32

	// column resizing by dragging the header edges
	userW      []float32 // width set by the user per column (0 = automatic)
	resizeCol  int       // column being resized while drag == dragCol
	resizeX0   float32   // pointer x when the resize drag started
	resizeW0   float32   // column width when the resize drag started
	resized    bool
	lastEdge   int       // column index+1 of the last header-edge click (0 = none)
	lastEdgeAt time.Time
	hoverX     float32
	hoverY     float32

	// cached theme values (reloaded in Refresh) so rendering never queries the theme
	themeGen                                        uint32
	colSel, colStripe, colPrimary, colFg, colHeader color.Color
	colSep, colBar                                  color.Color

	// ReadOnly mirrors MyGrid's rogrid. Default false; set false to do not allow editing.
	ReadOnly bool
	// EnterMovesNext: Enter moves the cursor to the next cell (Shift+Enter to the
	// previous one); after the last column it continues at the first column of
	// the next row. While editing, Enter commits the value first. Enter never
	// opens the editor: use F2, a double click, or just start typing.
	EnterMovesNext bool
	// EnterFirstCol / EnterLastCol (column keys, "" = the grid's first / last
	// column) are the ends of the range Enter walks through. Enter on
	// EnterLastCol (or any column after it) goes to EnterFirstCol of the next
	// row; Shift+Enter goes the other way. Use them to skip columns that need
	// no typing, e.g. an ID column on the left or a total on the right.
	EnterFirstCol string
	EnterLastCol  string
	// AutoAddRow: pressing Enter in the last cell of the last row adds a new
	// row (via AddRow) and puts the cursor in EnterFirstCol of it. Only on an
	// editable grid. default: False
	AutoAddRow bool
	// AddRow appends one empty row to YOUR slice and returns nil. New() sets
	// it for you (it appends a zero value of your struct). Replace it to add
	// defaults, e.g. a next ID, or to insert into a database.
	AddRow func() error
	// InsertRow inserts one empty row at source index at (rows from at onwards
	// move down by one). New() sets it for you; set it yourself for other data
	// sources. Without it "Insert row" is disabled.
	InsertRow func(at int) error
	// DeleteRow removes the row at source index at. Same rules as InsertRow.
	DeleteRow func(at int) error
	// BeforeRowDelete can veto a delete (e.g. to ask for confirmation or to
	// refuse deleting a posted row): return an error to cancel it.
	BeforeRowDelete func(idx int, row Row) error
	// OnRowDeleted fires after a row was deleted; idx is the index it had.
	OnRowDeleted func(idx int)
	// DisableContextMenu turns the right-click menu off. The menu has Copy
	// cell, Copy row(s) as CSV / PSV and Export CSV on every grid, plus Insert /
	// Delete / Append row on an editable one.
	DisableContextMenu bool
	// Window is the window the grid is shown in. Copy and Export need it (the
	// clipboard and the save dialog belong to a window): set it once after
	// creating the window, e.g. grid.Window = w.
	Window fyne.Window
	// ExportFileName is the name the save dialog starts with ("" = export.csv).
	ExportFileName string
	// OnRowAdded fires after a row was added or inserted (AutoAddRow, the
	// context menu, AppendRow, InsertRowAt) and the cursor moved to it.
	OnRowAdded func(idx int, row Row)
	Sortable       bool // click header to sort (default true)
	Striped        bool // alternate row shading (default true)
	Resizable      bool // drag a header edge to resize a column, double-click it to reset (default true)

	// FormatCell, if set, is asked for the display text of every visible cell,
	// the same way the cell func is asked for its value. row is the index in
	// YOUR data (stable across sorting), key is Column.Key. Return ok=false to
	// fall back to the column's own formatting (Formatter / Thousands /
	// Decimals). It only changes what is drawn: sorting, editing and change
	// detection keep using the raw values.
	FormatCell func(row int, key string) (text string, ok bool)

	// MultiSelect lets the user select several rows: Ctrl/Cmd+click toggles a
	// row, Shift+click (or Shift+Up/Down/PageUp/PageDown) selects a range,
	// Ctrl/Cmd+A selects all, Esc goes back to the cursor row alone. Default
	// true. With false the grid keeps its single-row behaviour.
	MultiSelect bool
	// CellSelect lets the user select a rectangle of cells: Shift+click,
	// Shift+arrow keys (also Left/Right/Home/End) or dragging the mouse over the
	// cells. Ctrl/Cmd+C copies the rectangle as tab-separated text and
	// Delete clears its editable cells. Default true. While CellSelect is on,
	// Shift+click and Shift+Up/Down make a cell rectangle instead of a row
	// range (Ctrl/Cmd+click and Ctrl/Cmd+A still select whole rows). With
	// false the grid keeps its row-only behaviour.
	CellSelect bool
	// OnCellSelectionChanged fires when the cell rectangle changed (also when
	// it shrank back to the single cursor cell).
	OnCellSelectionChanged func(r CellRange)
	// OnSelectionChanged fires when the set of selected rows changed. idxs are
	// indices in YOUR data, ascending. (OnSelected still reports the cursor row.)
	OnSelectionChanged func(idxs []int)

	OnSelected  func(idx int, row Row) // cursor moved to another row / row clicked
	OnActivated func(idx int, row Row) // double click on a row

	// OnColumnResized fires when the user finishes dragging a column edge.
	OnColumnResized func(col int, width float32)

	// StyleCell, if set, is asked for the style of every visible cell (like
	// FormatCell). row is the index in YOUR data, key is Column.Key. Return
	// ok=false for "default look". Because it is computed from your data it
	// never goes out of step when rows are inserted, deleted or sorted. Use it for rules
	// like "negative numbers are red" or "this column is bold".
	StyleCell func(row int, key string) (style CellStyle, ok bool)

	// OnEnter is called when Enter is pressed on the grid (idx = index in YOUR
	// data, key = the column's Key (its name), row = a snapshot of that row). While the inline
	// editor is open the value is committed first, then OnEnter is called.
	// Return true if you handled the key: the grid then does NOT move to the
	// next cell (EnterMovesNext / AutoAddRow are skipped). Return false to keep
	// the normal behaviour. Shift+Enter also calls it; check grid.ShiftDown().
	OnEnter func(idx int, key string, row Row) (handled bool)

	// CellWidget, if set, is asked for a widget to show INSTEAD of the text of
	// every visible cell (like FormatCell / StyleCell). row is the index in
	// YOUR data, key is Column.Key. Return nil for a normal text cell.
	//
	// Cells are pooled and reused while scrolling, so the grid hands you the
	// widget it last used in this pool cell as old (nil the first time, or
	// after it was nil). Reuse it when it is the right kind, update it for
	// this row and return it; otherwise create a new one:
	//
	//	g.CellWidget = func(row int, key string, old fyne.CanvasObject) fyne.CanvasObject {
	//		if key != "done" {
	//			return nil
	//		}
	//		chk, ok := old.(*widget.Check)
	//		if !ok {
	//			chk = widget.NewCheck("", nil)
	//		}
	//		chk.OnChanged = nil // setting Checked must not fire the handler
	//		chk.SetChecked(items[row].Done)
	//		chk.OnChanged = func(v bool) { items[row].Done = v }
	//		return chk
	//	}
	//
	// It is called on every repaint of a visible cell, so keep it cheap. The
	// widget fills the cell and handles its own clicks (the grid does not see
	// them, so the cursor does not move; the cell can still be reached with the
	// keyboard or by clicking next to the widget). The cell is not editable
	// while it shows a widget, and copy / export still use the cell text.
	CellWidget func(row int, key string, old fyne.CanvasObject) fyne.CanvasObject

	// OnKey is called for a plain key press (F5, Insert, Space, ...) before the
	// grid handles it. Return true if you handled it. idx = -1 when there is
	// no cursor row. Modified keys such as Ctrl+N go through AddShortcut.
	OnKey func(k *fyne.KeyEvent, idx int, key string, row Row) (handled bool)
	shortcuts map[string]func(idx int, key string, row Row)


	// BeforeCellChange can veto an edit: return an error to reject it
	// (the editor stays open and shows the error). Use it for validation.
	BeforeCellChange func(ev CellChange) error
	// OnCellChanged fires after a cell value was actually changed by the user.
	OnCellChanged func(ev CellChange)
}

var (
	_ fyne.Focusable  = (*DataGrid)(nil)
	_ fyne.Tabbable   = (*DataGrid)(nil)
	_ fyne.Tappable   = (*DataGrid)(nil)
	_ fyne.Scrollable = (*DataGrid)(nil)
	_ fyne.Draggable  = (*DataGrid)(nil)
	_ desktop.Keyable = (*DataGrid)(nil)
	_ fyne.SecondaryTappable = (*DataGrid)(nil)
	_ desktop.Hoverable  = (*DataGrid)(nil)
	_ desktop.Cursorable = (*DataGrid)(nil)
	_ desktop.Mouseable  = (*DataGrid)(nil)
	_ fyne.Shortcutable  = (*DataGrid)(nil)
)

// NewDataGrid creates a grid from a row-count func and a cell func (the closures
// built by New from your struct type).
func NewDataGrid(cols []Column, length func() int, cell func(row int, key string) any) *DataGrid {
	g := &DataGrid{
		cols: cols,
		src: emptySource{},
		selected: -1,
		anchor: -1,
		anchorCol: -1,
		MultiSelect: true,
		CellSelect: true,
		sortCol: -1,
		sortAsc: true,
		ReadOnly: false,
		EnterMovesNext: true,
		AutoAddRow: false,
		Sortable: true,
		Striped: true,
		Resizable: true,
		colX:  make([]float32, len(cols)),
		colW:  make([]float32, len(cols)),
		userW: make([]float32, len(cols)),
	}
	if length != nil && cell != nil {
		g.lengthFn, g.cellFn = length, cell
		g.src = newFuncSource(length, cell, nil)
		g.viewStale = true // built lazily on first layout, like widget.Table
	}
	g.ExtendBaseWidget(g)
	g.loadColors()

	e := newCellEntry()
	e.Validator = func(string) error { return nil } // lets SetValidationError work
	e.OnChanged = func(s string) {
		if g.quiet || g.edit == nil {
			return
		}
		g.edit.text = s
	}
	e.nav = g.onEditorNav
	e.blur = g.onEditorBlur
	e.Hide()
	g.entry = e
	return g
}

func (g *DataGrid) loadColors() {
	g.colSel = theme.Color(theme.ColorNameSelection)
	g.colStripe = theme.Color(theme.ColorNameInputBackground)
	g.colPrimary = theme.Color(theme.ColorNamePrimary)
	g.colFg = theme.Color(theme.ColorNameForeground)
	g.colHeader = theme.Color(theme.ColorNameHeaderBackground)
	g.colSep = theme.Color(theme.ColorNameSeparator)
	g.colBar = theme.Color(theme.ColorNameScrollBar)
}

// Refresh works like widget.Table's Refresh: call it after your data changed
// and the grid re-reads it (row count, values, and the sort order if a sort is
// active). Fyne also calls it on theme changes. Internal state changes use
// redraw() instead, which skips all of this.
func (g *DataGrid) Refresh() {
	g.themeGen++
	g.loadColors()
	g.viewStale = false
	g.resync()
	g.BaseWidget.Refresh()
}

// resync rebuilds the display order from the source (new length, new values)
// and keeps selection, scroll position and an open editor where still valid.
func (g *DataGrid) resync() {
	g.rebuildView()
	n := len(g.view)
	if g.selected >= n {
		g.selected = -1
	}
	g.trimSelection(n)
	if g.edit != nil && g.edit.row >= n {
		g.edit = nil
	}
	g.clampTop()
}

func (g *DataGrid) redraw() { g.BaseWidget.Refresh() }

// setEditFunc makes the grid editable (also needs ReadOnly = false). It is
// called with the already-parsed value (decimal.Decimal, time.Time or string,
// depending on the column type) when the user commits a cell. Return an error
// to reject the value; the editor stays open and shows it. Only for grids
// created by New. Pass nil to make it read-only.
func (g *DataGrid) setEditFunc(set func(row int, key string, v any) error) {
	if g.lengthFn == nil || g.cellFn == nil {
		return
	}
	g.src = newFuncSource(g.lengthFn, g.cellFn, set)
}

// ensureView makes sure view / pos match the data source. Besides the lazy
// first build it also catches data that grew or shrank behind the grid's back
// (e.g. you appended to your slice and called SelectIndex before any
// Reload / repaint), so view and pos are never shorter than the source.
func (g *DataGrid) ensureView() {
	if g.viewStale {
		g.viewStale = false
		g.rebuildView()
		return
	}
	if g.src.Len() != len(g.view) || len(g.pos) != len(g.view) {
		g.resync()
	}
}

func (g *DataGrid) CreateRenderer() fyne.WidgetRenderer {
	return newDataGridRenderer(g)
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Reload re-reads the source after rows were added/removed/changed behind the
// grid's back (re-applies the sort, keeps selection + scroll where possible).
// Call it on the UI goroutine.
func (g *DataGrid) Reload() {
	g.edit = nil
	g.rebuildView()
	if g.selected >= len(g.view) {
		g.selected = -1
	}
	g.trimSelection(len(g.view))
	g.clampTop()
	g.redraw()
}

// Repaint redraws after only *displayed values* changed (same row count, and
// the sorted column unchanged). Cheaper than Reload because it skips the sort.
func (g *DataGrid) Repaint() { g.redraw() }

// RepaintRow redraws just one row after its displayed values changed.
// rowIdx is the index into your data source (the same index SetCellValue,
// CellChange.RowIndex and SelectIndex use), not the on-screen position, so it
// is correct even when the grid is sorted. Rows that are scrolled out of view
// are ignored (they are drawn from fresh data when they scroll in).
//
// Like Repaint it does not re-sort: if you changed the value of the column
// the grid is sorted by, call Reload instead. Call it on the UI goroutine.
func (g *DataGrid) RepaintRow(rowIdx int) {
	r := g.liveRenderer()
	if r == nil {
		return
	}
	k, ok := r.poolRow(rowIdx)
	if !ok {
		return
	}
	r.syncRow(k, g.viewW, false)
}

// RepaintCell redraws just one cell, identified by data-source row index and
// column key. Same rules as RepaintRow.
func (g *DataGrid) RepaintCell(rowIdx int, key string) {
	r := g.liveRenderer()
	if r == nil {
		return
	}
	c := g.colIndex(key)
	if c < 0 {
		return
	}
	k, ok := r.poolRow(rowIdx)
	if !ok {
		return
	}
	r.syncCell(k, c, rowIdx, g.headerH+float32(k)*g.rowH, false)
}

// colIndex returns the index of the column with this key, or -1.
func (g *DataGrid) colIndex(key string) int {
	for i, c := range g.cols {
		if c.Key == key {
			return i
		}
	}
	return -1
}

// liveRenderer returns the renderer if a targeted repaint is safe. When it
// isn't (grid not shown yet, or the columns changed and the renderer hasn't
// rebuilt), it schedules a normal full redraw instead and returns nil.
func (g *DataGrid) liveRenderer() *dataGridRenderer {
	r := g.rend
	if r == nil {
		return nil // never shown: nothing on screen to repaint
	}
	if r.colGen != g.colsGen || r.seenGen != g.themeGen || g.viewStale || len(g.view) != g.src.Len() {
		g.redraw() // pools / layout are stale: let the full path handle it
		return nil
	}
	return r
}

// poolRow maps a source row index to its slot in the renderer's row pool.
// ok is false when the row isn't currently on screen.
func (r *dataGridRenderer) poolRow(rowIdx int) (k int, ok bool) {
	g := r.g
	if rowIdx < 0 || rowIdx >= len(g.pos) {
		return 0, false
	}
	k = g.pos[rowIdx] - g.topRow
	if k < 0 || k >= len(r.rowBg) || k >= g.visRows {
		return 0, false
	}
	return k, true
}

// SetColumns replaces the column definitions (e.g. for a different report).
// The sort is reset; the data source is kept.
func (g *DataGrid) SetColumns(cols []Column) {
	g.edit = nil
	g.cols = cols
	g.colX = make([]float32, len(cols))
	g.colW = make([]float32, len(cols))
	g.userW = make([]float32, len(cols))
	g.colsGen++
	g.sortCol, g.sortAsc = -1, true
	g.leftX = 0
	g.cellOn, g.anchorCol = false, -1
	g.curCol = clampInt(g.curCol, 0, maxInt(len(cols)-1, 0))
	g.rebuildView()
	g.redraw()
}

func (g *DataGrid) Columns() []Column { return g.cols }

// ColumnWidth returns the width a column currently has, in px.
func (g *DataGrid) ColumnWidth(col int) float32 {
	if col < 0 || col >= len(g.colW) {
		return 0
	}
	return g.colW[col]
}

// SetColumnWidth pins a column to w px (what dragging its header edge does).
// w = 0 puts it back to automatic sizing.
func (g *DataGrid) SetColumnWidth(col int, w float32) {
	if col < 0 || col >= len(g.userW) {
		return
	}
	if w > 0 && w < minUserColW {
		w = minUserColW
	}
	if w < 0 {
		w = 0
	}
	g.userW[col] = w
	g.redraw()
}

// ResetColumnWidths undoes all manual column resizing.
func (g *DataGrid) ResetColumnWidths() {
	for i := range g.userW {
		g.userW[i] = 0
	}
	g.redraw()
}

func (g *DataGrid) RowCount() int { return g.src.Len() }

// SelectedIndex returns the row index in your data source (-1 if none).
func (g *DataGrid) SelectedIndex() int { return g.selected }
func (g *DataGrid) CursorCol() int     { return g.curCol }

func (g *DataGrid) SelectedRow() (Row, bool) {
	if g.selected < 0 || g.selected >= g.src.Len() {
		return nil, false
	}
	return g.rowOf(g.selected), true
}

// rowOf builds a Row (key -> value) snapshot for event callbacks.
func (g *DataGrid) rowOf(i int) Row {
	r := make(Row, len(g.cols))
	for _, c := range g.cols {
		r[c.Key] = g.src.Value(i, c.Key)
	}
	return r
}

func (g *DataGrid) editable() bool {
	_, ok := g.src.(EditableSource)
	return ok
}

// SelectIndex selects a row by its index in the data and scrolls to it.
func (g *DataGrid) SelectIndex(idx int) {
	g.ensureView()
	if idx < 0 || idx >= g.src.Len() {
		had := g.selected >= 0 || g.selOn
		g.selected = -1
		g.dropSelection()
		g.redraw()
		if had {
			g.fireSelection()
		}
		return
	}
	if idx >= len(g.pos) { // source grew while the view was being rebuilt: be safe
		return
	}
	g.moveTo(g.pos[idx], g.curCol)
}

// Sum adds up a numeric column (handy for Dr / Cr totals).
func (g *DataGrid) Sum(key string) decimal.Decimal {
	t := decimal.Zero
	for i, n := 0, g.src.Len(); i < n; i++ {
		d, _ := toDecimal(g.src.Value(i, key))
		t = t.Add(d)
	}
	return t
}

// SetCellValue changes a value from code (no events fired) and repaints it.
// rowIdx is the index in YOUR data, key the column key. The value is converted
// to the column's type first (number columns take decimal.Decimal, ints or a
// numeric string; int columns take ints or a whole-number string; date columns
// take time.Time or a date string). It returns an error if the data source is
// read-only, the row / column doesn't exist or the value doesn't fit. Floats
// are rejected on purpose: use decimal.NewFromFloat. Needs an EditableSource.
// Call it on the UI goroutine (inside fyne.Do when you are on another one).
func (g *DataGrid) SetCellValue(rowIdx int, key string, v any) error {
	es, ok := g.src.(EditableSource)
	if !ok {
		return fmt.Errorf("this data source is read-only")
	}
	if rowIdx < 0 || rowIdx >= g.src.Len() {
		return fmt.Errorf("row %d does not exist", rowIdx)
	}
	ci := g.colIndex(key)
	if ci < 0 {
		return fmt.Errorf("unknown column %q", key)
	}
	val, err := g.cols[ci].coerce(v)
	if err != nil {
		return err
	}
	if err := es.SetValue(rowIdx, key, val); err != nil {
		return err
	}
	if g.sortCol == ci {
		g.rebuildView() // the sort order depends on this value
		g.redraw()
		return nil
	}
	if r := g.liveRenderer(); r != nil {
		if _, onScreen := r.poolRow(rowIdx); onScreen {
			g.RepaintCell(rowIdx, key)
			return nil
		}
	}
	g.redraw()
	return nil
}

// ---------------------------------------------------------------------------
// Row operations (also used by the right-click context menu)
// ---------------------------------------------------------------------------

var errRowOpUnavailable = fmt.Errorf("the grid is read-only or its data source can't add / remove rows")

func (g *DataGrid) rowOpsAllowed() bool { return !g.ReadOnly && g.editable() }

func (g *DataGrid) canAppend() bool { return g.rowOpsAllowed() && g.AddRow != nil }

func (g *DataGrid) canInsert() bool { return g.rowOpsAllowed() && g.InsertRow != nil }

func (g *DataGrid) canDelete() bool { return g.rowOpsAllowed() && g.DeleteRow != nil }

// settleEditor applies an open editor, or drops it if its text is invalid.
func (g *DataGrid) settleEditor() {
	if g.edit != nil && !g.commitEdit() {
		g.endEdit(false)
	}
}

// structureChanged re-reads the source after rows were added / inserted /
// deleted. Selection is left to the caller (source indices shifted).
func (g *DataGrid) structureChanged() {
	g.edit = nil
	g.viewStale = false
	g.lastTap = time.Time{} // a double click must not straddle a shifted row
	g.dropSelection()       // selected source indices are stale now
	g.rebuildView()
	g.clampTop()
}

// selectSource puts the cursor on source row ri (keeping the cursor column).
func (g *DataGrid) selectSource(ri int) {
	if ri < 0 || ri >= len(g.pos) {
		g.selected = -1
		g.dropSelection()
		g.redraw()
		return
	}
	g.moveTo(g.pos[ri], g.curCol)
}

// AppendRow adds an empty row at the end of your data and selects it.
func (g *DataGrid) AppendRow() error {
	if !g.canAppend() {
		return errRowOpUnavailable
	}
	g.settleEditor()
	before := g.src.Len()
	if err := g.AddRow(); err != nil {
		return err
	}
	g.structureChanged()
	n := g.src.Len()
	if n <= before {
		g.redraw()
		return nil
	}
	g.selectSource(n - 1)
	if g.OnRowAdded != nil {
		g.OnRowAdded(n-1, g.rowOf(n-1))
	}
	return nil
}

// InsertRowAt inserts an empty row at source index at (pass RowCount() to
// append) and selects it. With an active sort the row shows up wherever its
// (empty) values sort to, and the cursor follows it there.
func (g *DataGrid) InsertRowAt(at int) error {
	if !g.canInsert() {
		return errRowOpUnavailable
	}
	g.settleEditor()
	if at < 0 || at > g.src.Len() {
		return fmt.Errorf("row %d does not exist", at)
	}
	before := g.src.Len()
	if err := g.InsertRow(at); err != nil {
		return err
	}
	g.structureChanged()
	if g.src.Len() <= before {
		g.redraw()
		return nil
	}
	g.selectSource(at)
	if g.OnRowAdded != nil {
		g.OnRowAdded(at, g.rowOf(at))
	}
	return nil
}

// DeleteRowAt removes the row at source index at. The cursor stays at the same
// screen position (on the next row, or the last one if the last row went).
func (g *DataGrid) DeleteRowAt(at int) error {
	if !g.canDelete() {
		return errRowOpUnavailable
	}
	g.settleEditor()
	if at < 0 || at >= g.src.Len() {
		return fmt.Errorf("row %d does not exist", at)
	}
	if g.BeforeRowDelete != nil {
		if err := g.BeforeRowDelete(at, g.rowOf(at)); err != nil {
			return err
		}
	}
	sel := g.selected // source index of the cursor row
	vr := g.cursorViewRow() // remember where the cursor is on screen
	if vr < 0 {
		vr = g.pos[at]
	}
	before := g.src.Len()
	if err := g.DeleteRow(at); err != nil {
		return err
	}
	g.structureChanged()
	if n := g.src.Len(); n == 0 {
		g.selected = -1
		g.redraw()
	} else if n < before && sel >= 0 && sel != at {
		if sel > at {
			sel-- // rows below the deleted one shifted up
		}
		g.selected = sel // the cursor row itself wasn't deleted: keep it
		g.ensureVisible(g.pos[sel])
		g.redraw()
	} else if n < before {
		g.selected = g.view[clampInt(vr, 0, n-1)]
		g.ensureVisible(g.pos[g.selected])
		g.redraw()
		if g.OnSelected != nil {
			g.OnSelected(g.selected, g.rowOf(g.selected))
		}
	} else {
		g.redraw()
	}
	g.fireSelection()
	if g.OnRowDeleted != nil {
		g.OnRowDeleted(at)
	}
	return nil
}

// TappedSecondary (right click / long press) selects the cell under the
// pointer and opens the menu: Copy cell(s), Paste cell(s), Copy row(s) as CSV / PSV, Export CSV,
// and on an editable grid Insert row, Delete row(s), Append row.
func (g *DataGrid) TappedSecondary(ev *fyne.PointEvent) {
	if g.DisableContextMenu || g.rowH <= 0 || ev.Position.Y < g.headerH {
		return
	}
	target := -1
	k := int((ev.Position.Y - g.headerH) / g.rowH)
	if vr := g.topRow + k; k >= 0 && vr < len(g.view) {
		target = g.view[vr] // the row that was right-clicked
	}
	// Right-clicking inside a multi-row selection keeps it (and the menu then
	// acts on all of it); anywhere else it selects just that cell.
	nSel := 0
	inCells := false // right-click inside the cell rectangle keeps it
	if g.cellOn && target >= 0 {
		if c := g.colAt(ev.Position.X); c >= 0 {
			inCells = g.IsCellSelected(target, c)
		}
	}
	if inCells {
		// keep the rectangle exactly as it is; the menu then copies all of it
	} else if g.MultiSelect && target >= 0 && g.IsRowSelected(target) && g.SelectedCount() > 1 {
		nSel = g.SelectedCount()
		g.moveCursorKeepSelection(g.pos[target], g.colAt(ev.Position.X))
	} else {
		g.mods = 0
		g.selectAt(ev.Position.X, ev.Position.Y) // no-op over empty space below the rows
	}

	at := ev.AbsolutePosition
	report := func(err error) {
		if err == nil {
			return
		}
		if c := fyne.CurrentApp().Driver().CanvasForObject(g); c != nil {
			widget.ShowPopUpAtPosition(widget.NewLabel(err.Error()), c, at) // closes on click away
		}
	}
	insert := fyne.NewMenuItem("Insert row", func() { report(g.InsertRowAt(target)) })
	delLabel, delFn := "Delete row", func() { report(g.DeleteRowAt(target)) }
	if nSel > 1 {
		delLabel, delFn = fmt.Sprintf("Delete %d rows", nSel), func() { report(g.DeleteSelectedRows()) }
	}
	del := fyne.NewMenuItem(delLabel, delFn)
	app := fyne.NewMenuItem("Append row", func() { report(g.AppendRow()) })
	insert.Disabled = target < 0 || !g.canInsert()
	del.Disabled = target < 0 || !g.canDelete()
	app.Disabled = !g.canAppend()

	rowsWord := "row"
	if nSel > 1 {
		rowsWord = fmt.Sprintf("%d rows", nSel)
	}
	copyCell := fyne.NewMenuItem("Copy cell", func() { report(g.CopyCursorCell()) })
	if inCells {
		r := g.SelectedCellRange()
		copyCell = fyne.NewMenuItem(fmt.Sprintf("Copy %d cells", len(r.Rows)*len(r.Cols)), func() { report(g.CopySelectedCells()) })
	}
	copyCSV := fyne.NewMenuItem("Copy "+rowsWord+" as CSV", func() { report(g.CopySelectedCSV()) })
	copyPSV := fyne.NewMenuItem("Copy "+rowsWord+" as PSV", func() { report(g.CopySelectedPSV()) })
	export := fyne.NewMenuItem("Export CSV…", func() { report(g.ExportCSV(false)) })
	copyCell.Disabled, copyCSV.Disabled, copyPSV.Disabled = target < 0, target < 0, target < 0
	export.Disabled = g.src.Len() == 0

	pasteLabel := "Paste cell"
	if inCells {
		if r := g.SelectedCellRange(); len(r.Rows)*len(r.Cols) > 1 {
			pasteLabel = "Paste cells"
		}
	}
	paste := fyne.NewMenuItem(pasteLabel, func() { report(g.PasteCells()) })
	paste.Disabled = target < 0 || !g.canPaste()

	items := []*fyne.MenuItem{copyCell, paste, copyCSV, copyPSV, fyne.NewMenuItemSeparator(), export}
	if nSel > 1 {
		items = append(items, fyne.NewMenuItem("Export selected rows as CSV…", func() { report(g.ExportCSV(true)) }))
	}
	if g.rowOpsAllowed() {
		items = append(items, fyne.NewMenuItemSeparator(), insert, del, fyne.NewMenuItemSeparator(), app)
	}

	c := fyne.CurrentApp().Driver().CanvasForObject(g)
	if c == nil {
		return
	}
	widget.ShowPopUpMenuAtPosition(fyne.NewMenu("", items...), c, at)
}

// SortBy sorts by column index; calling again on the same column flips direction.
func (g *DataGrid) SortBy(col int) {
	if col < 0 || col >= len(g.cols) {
		return
	}
	if g.sortCol == col {
		g.sortAsc = !g.sortAsc
	} else {
		g.sortCol, g.sortAsc = col, true
	}
	g.collapseCells() // a rectangle would change shape when the rows move
	g.rebuildView()
	if vr := g.cursorViewRow(); vr >= 0 {
		g.ensureVisible(vr)
	}
	g.redraw()
}

// EditCell moves the cursor to the cell and opens its editor.
func (g *DataGrid) EditCell(rowIdx, col int) {
	g.ensureView()
	if rowIdx < 0 || rowIdx >= g.src.Len() {
		return
	}
	if g.edit != nil && !g.commitEdit() {
		return
	}
	g.moveTo(g.pos[rowIdx], col)
	g.beginEdit("", false)
}

// CommitEdit applies an open editor (call it before saving a form).
// Returns false if the entered value was rejected.
func (g *DataGrid) CommitEdit() bool {
	if g.edit == nil {
		return true
	}
	return g.commitEdit()
}

// CancelEdit discards an open editor.
func (g *DataGrid) CancelEdit() {
	if g.edit != nil {
		g.endEdit(true)
	}
}

// ---------------------------------------------------------------------------
// Sorting / view
// ---------------------------------------------------------------------------

func (g *DataGrid) rebuildView() {
	n := g.src.Len()
	g.view = make([]int, n)
	for i := range g.view {
		g.view[i] = i
	}
	if g.sortCol >= 0 && g.sortCol < len(g.cols) {
		// Read each value once up front instead of inside the comparator.
		col := g.cols[g.sortCol]
		var cmp func(i, j int) int
		switch col.Type {
		case ColNumber:
			k := make([]decimal.Decimal, n)
			for i := range k {
				k[i], _ = toDecimal(g.src.Value(i, col.Key))
			}
			cmp = func(i, j int) int { return k[i].Cmp(k[j]) }
		case ColInt:
			k := make([]int64, n)
			for i := range k {
				k[i], _ = toInt64(g.src.Value(i, col.Key))
			}
			cmp = func(i, j int) int {
				switch {
				case k[i] < k[j]:
					return -1
				case k[i] > k[j]:
					return 1
				}
				return 0
			}
		case ColDate:
			k := make([]time.Time, n)
			for i := range k {
				k[i], _ = toTime(g.src.Value(i, col.Key))
			}
			cmp = func(i, j int) int {
				switch {
				case k[i].Before(k[j]):
					return -1
				case k[i].After(k[j]):
					return 1
				}
				return 0
			}
		default:
			k := make([]string, n)
			for i := range k {
				k[i] = strings.ToLower(textOf(g.src.Value(i, col.Key)))
			}
			cmp = func(i, j int) int { return strings.Compare(k[i], k[j]) }
		}
		sort.SliceStable(g.view, func(a, b int) bool {
			c := cmp(g.view[a], g.view[b])
			if !g.sortAsc {
				c = -c
			}
			return c < 0
		})
	}
	g.pos = make([]int, n)
	for p, ri := range g.view {
		g.pos[ri] = p
	}
}

// ---------------------------------------------------------------------------
// Layout metrics, scrolling
// ---------------------------------------------------------------------------

// layoutMetrics recomputes row height, how many rows fit, the scrollbar
// and the column geometry for the given widget size.
func (g *DataGrid) layoutMetrics(size fyne.Size) {
	g.ensureView()
	if g.src.Len() != len(g.view) { // data grew/shrank without Refresh/Reload
		g.resync()
	}
	if ts := theme.TextSize(); g.rowH == 0 || ts != g.textSize {
		g.textSize = ts
		g.themeGen++ // text size changed: restyle + invalidate truncation caches
		// Row height = whatever the inline editor needs, so it never clips.
		eh := widget.NewEntry().MinSize().Height
		th := fyne.MeasureText("Hg", ts, fyne.TextStyle{Bold: true}).Height + 2*theme.Padding()
		g.rowH = fyne.Max(eh, th)
		g.headerH = g.rowH
	}

	// Scrollbars depend on each other (a bar takes space, which can make the
	// other one necessary), so settle them in a few passes.
	bar := theme.ScrollBarSize()
	hasBar, hasH := false, false
	var visRows int
	var total, content float32
	for pass := 0; pass < 3; pass++ {
		var sbW, sbH float32
		if hasBar {
			sbW = bar
		}
		if hasH {
			sbH = bar
		}
		visRows = 0
		if body := size.Height - g.headerH - sbH; body > 0 {
			visRows = int(body / g.rowH)
		}
		nBar := visRows > 0 && len(g.view) > visRows
		total = fyne.Max(size.Width-sbW, 0)
		content = g.computeColumns(total)
		nH := content > total+0.5
		if nBar == hasBar && nH == hasH {
			break
		}
		hasBar, hasH = nBar, nH
	}
	g.visRows, g.hasBar, g.hasH = visRows, hasBar, hasH
	g.sbW, g.sbH = 0, 0
	if hasBar {
		g.sbW = bar
	}
	if hasH {
		g.sbH = bar
	}
	g.viewW, g.contentW = total, content
	g.clampLeft()
	x := -g.leftX
	for i := range g.cols {
		g.colX[i] = x
		x += g.colW[i]
	}
	g.clampTop()
}

// computeColumns fills colW. WidthPct is taken literally as a percentage of
// the available width: IntCol("ccy", "Currency", 10) gets exactly 10% of the
// grid width. Percentages that add up to less than 100 leave the remaining
// space empty on the right; if they add up to more than 100 the grid scrolls
// horizontally. A column's MinWidth (only when set > 0) is the one exception:
// the column never gets narrower than that. Columns resized by the user keep
// the width they were dragged to. Returns the total content width.
func (g *DataGrid) computeColumns(total float32) float32 {
	n := len(g.cols)
	if n == 0 {
		return 0
	}
	var content float32
	for i, c := range g.cols {
		var w float32
		if len(g.userW) == n && g.userW[i] > 0 { // width chosen by the user
			w = g.userW[i]
		} else {
			w = total * c.WidthPct / 100
			if c.MinWidth > 0 && w < c.MinWidth {
				w = c.MinWidth
			}
		}
		g.colW[i] = w
		content += w
	}
	return content
}

func (g *DataGrid) clampLeft() {
	g.leftX = fyne.Min(fyne.Max(g.leftX, 0), fyne.Max(g.contentW-g.viewW, 0))
}

// ensureColVisible scrolls sideways (if needed) so column c is on screen.
func (g *DataGrid) ensureColVisible(c int) {
	if !g.hasH || c < 0 || c >= len(g.colX) {
		return
	}
	cx := g.colX[c] + g.leftX // position in content coordinates
	if cx+g.colW[c] > g.leftX+g.viewW {
		g.leftX = cx + g.colW[c] - g.viewW
	}
	if cx < g.leftX {
		g.leftX = cx
	}
	g.clampLeft()
}

// colVisible returns the part of column c that is inside the viewport
// (screen coordinates). ok is false when the column is scrolled out of view.
func (g *DataGrid) colVisible(c int) (x0, x1 float32, ok bool) {
	x0 = fyne.Max(g.colX[c], 0)
	x1 = fyne.Min(g.colX[c]+g.colW[c], g.viewW)
	return x0, x1, x1 > x0
}

func (g *DataGrid) clampTop() {
	maxTop := len(g.view) - g.visRows
	if maxTop < 0 {
		maxTop = 0
	}
	g.topRow = clampInt(g.topRow, 0, maxTop)
}

// ensureVisible scrolls (if needed) so display row vr is on screen.
func (g *DataGrid) ensureVisible(vr int) {
	vis := maxInt(g.visRows, 1)
	if vr < g.topRow {
		g.topRow = vr
	} else if vr >= g.topRow+vis {
		g.topRow = vr - vis + 1
	}
	g.clampTop()
}

func (g *DataGrid) colAt(x float32) int {
	if x < 0 || x >= g.viewW {
		return -1
	}
	for i := range g.cols {
		if x >= g.colX[i] && x < g.colX[i]+g.colW[i] {
			return i
		}
	}
	return -1
}

// edgeGrabZone is how far from a header edge a pointer still grabs it
// (wider on touch devices, where a finger is much less precise).
func (g *DataGrid) edgeGrabZone() float32 {
	if fyne.CurrentDevice().IsMobile() {
		return 18
	}
	return edgeGrab
}

// edgeAt returns the column whose right edge is within grabbing distance of x
// (header coordinates), or -1.
func (g *DataGrid) edgeAt(x float32) int {
	if !g.Resizable {
		return -1
	}
	grab := g.edgeGrabZone()
	for i := range g.cols {
		e := g.colX[i] + g.colW[i]
		if e > 0 && e <= g.viewW+0.5 && x >= e-grab && x <= e+grab {
			return i
		}
	}
	return -1
}

// MouseIn / MouseMoved / MouseOut track the pointer so Cursor() can show the
// resize cursor over a header edge.
func (g *DataGrid) MouseIn(ev *desktop.MouseEvent) { g.hoverX, g.hoverY = ev.Position.X, ev.Position.Y }
func (g *DataGrid) MouseMoved(ev *desktop.MouseEvent) { g.hoverX, g.hoverY = ev.Position.X, ev.Position.Y }
func (g *DataGrid) MouseOut()                      { g.hoverX, g.hoverY = -1, -1 }

func (g *DataGrid) Cursor() desktop.Cursor {
	if g.drag == dragCol || (g.hoverY >= 0 && g.hoverY < g.headerH && g.edgeAt(g.hoverX) >= 0) {
		return desktop.CrosshairCursor
	}
	return desktop.DefaultCursor
}

// Scrolled handles the mouse wheel / touchpad (vertical, and sideways with
// Shift+wheel or a horizontal touchpad swipe).
func (g *DataGrid) Scrolled(ev *fyne.ScrollEvent) {
	changed := false

	// sideways: DX, or the plain wheel when there are no rows to scroll
	dx := ev.Scrolled.DX
	if dx == 0 && !g.hasBar && g.hasH {
		dx = ev.Scrolled.DY
	}
	if dx != 0 && g.hasH {
		before := g.leftX
		g.leftX -= dx / 10 * scrollPxPerNotch
		g.clampLeft()
		changed = g.leftX != before
	}

	// Fyne: positive DY = scroll up. ~10 units per wheel notch.
	g.scrollAcc -= ev.Scrolled.DY / 10 * scrollRowsPerNotch
	if n := int(g.scrollAcc); n != 0 {
		g.scrollAcc -= float32(n)
		before := g.topRow
		g.topRow += n
		g.clampTop()
		if g.topRow != before {
			changed = true
		}
	}
	if changed {
		g.redraw()
	}
}

// Dragged lets the scrollbar thumbs be dragged.
func (g *DataGrid) Dragged(ev *fyne.DragEvent) {
	size := g.Size()
	if g.drag == dragNone {
		sx := ev.Position.X - ev.Dragged.DX
		sy := ev.Position.Y - ev.Dragged.DY
		switch {
		case sy < g.headerH && g.edgeAt(sx) >= 0:
			g.drag = dragCol
			g.resizeCol = g.edgeAt(sx)
			g.resizeX0, g.resizeW0 = sx, g.colW[g.resizeCol]
			g.resized = false
		case g.hasH && sy >= size.Height-g.sbH:
			g.drag = dragHBar
		case g.hasBar && sx >= size.Width-g.sbW && sy >= g.headerH && sy < size.Height-g.sbH:
			g.drag = dragBar
		default:
			if fyne.CurrentDevice().IsMobile() {
				g.drag = dragContent
			} else {
				g.drag = dragOther
				g.selectAt(sx, sy) // a click that wobbled is a drag to Fyne: still select + focus
				g.dragCells = g.startsCellDrag(sx, sy)
			}
		}
	}
	switch g.drag {
	case dragOther:
		g.dragSelect(ev.Position)
	case dragContent:
		// the content follows the finger (no fling / inertia)
		g.scrollAcc -= ev.Dragged.DY / g.rowH
		if n := int(g.scrollAcc); n != 0 {
			g.scrollAcc -= float32(n)
			g.topRow += n
			g.clampTop()
		}
		if g.hasH {
			g.leftX -= ev.Dragged.DX
			g.clampLeft()
		}
		g.redraw()
	case dragCol:
		w := fyne.Max(g.resizeW0+ev.Position.X-g.resizeX0, minUserColW)
		if g.userW[g.resizeCol] != w {
			g.userW[g.resizeCol] = w
			g.resized = true
			g.redraw()
		}
	case dragBar:
		track := size.Height - g.headerH - g.sbH - g.thumbH
		maxTop := len(g.view) - g.visRows
		if track <= 0 || maxTop <= 0 {
			return
		}
		f := (ev.Position.Y - g.headerH - g.thumbH/2) / track
		before := g.topRow
		g.topRow = int(f*float32(maxTop) + 0.5)
		g.clampTop()
		if g.topRow != before {
			g.redraw()
		}
	case dragHBar:
		track := g.viewW - g.hthumbW
		maxLeft := g.contentW - g.viewW
		if track <= 0 || maxLeft <= 0 {
			return
		}
		f := (ev.Position.X - g.hthumbW/2) / track
		before := g.leftX
		g.leftX = f * maxLeft
		g.clampLeft()
		if g.leftX != before {
			g.redraw()
		}
	}
}

func (g *DataGrid) DragEnd() {
	if g.drag == dragCol && g.resized && g.OnColumnResized != nil {
		g.OnColumnResized(g.resizeCol, g.userW[g.resizeCol])
	}
	g.drag, g.resized, g.dragCells = dragNone, false, false
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

const doubleClickGap = 400 * time.Millisecond

// Tapped handles header sorting, scrollbar-track paging and cell clicks.
//
// NOTE: DataGrid must NOT implement fyne.DoubleTappable. Fyne holds back every
// single tap for ~300ms on objects that do, to see if a second one follows,
// which makes clicks feel slow. Double-clicks are detected here instead.
func (g *DataGrid) Tapped(ev *fyne.PointEvent) {
	if g.rowH <= 0 {
		return
	}
	p := ev.Position

	if p.Y < g.headerH { // header: sort
		if i := g.edgeAt(p.X); i >= 0 { // on a column edge: double-click = automatic width
			now := time.Now()
			if g.lastEdge == i+1 && now.Sub(g.lastEdgeAt) < doubleClickGap {
				g.lastEdge = 0
				g.userW[i] = 0
				g.redraw()
			} else {
				g.lastEdge, g.lastEdgeAt = i+1, now
			}
			return
		}
		col := g.colAt(p.X)
		if col < 0 || !g.Sortable {
			return
		}
		if g.edit != nil && !g.commitEdit() {
			return
		}
		g.SortBy(col)
		return
	}

	if g.hasH && p.Y >= g.Size().Height-g.sbH { // horizontal scrollbar track: page left/right
		step := fyne.Max(g.viewW*0.9, 1)
		if p.X < g.hthumbX {
			g.leftX -= step
		} else if p.X > g.hthumbX+g.hthumbW {
			g.leftX += step
		}
		g.clampLeft()
		g.redraw()
		return
	}

	if g.hasBar && p.X >= g.Size().Width-g.sbW { // scrollbar track: page up/down
		step := maxInt(g.visRows-1, 1)
		if p.Y < g.thumbY {
			g.topRow -= step
		} else if p.Y > g.thumbY+g.thumbH {
			g.topRow += step
		}
		g.clampTop()
		g.redraw()
		return
	}

	k := int((p.Y - g.headerH) / g.rowH)
	vr := g.topRow + k
	col := g.colAt(p.X)
	if k < 0 || vr >= len(g.view) || col < 0 {
		return
	}
	src, now := g.view[vr], time.Now()
	double := g.lastTapRow == src && g.lastTapCol == col && now.Sub(g.lastTap) < doubleClickGap
	g.lastTapRow, g.lastTapCol, g.lastTap = src, col, now

	g.clickCell(vr, col)
	if double {
		g.lastTap = time.Time{}
		g.activate()
	}
}

// clickCell is what a mouse click on a cell does: commit any open editor,
// move the cursor there and give the grid keyboard focus.
func (g *DataGrid) clickCell(vr, col int) {
	if vr < 0 || vr >= len(g.view) || col < 0 || col >= len(g.cols) {
		return
	}
	if g.edit != nil && !g.commitEdit() {
		g.endEdit(false)
	}
	prev, wasMulti := g.selected, g.selOn
	ri := g.view[vr]
	ctrl := g.mods&(fyne.KeyModifierControl|fyne.KeyModifierSuper) != 0
	shift := g.mods&fyne.KeyModifierShift != 0
	switch {
	case g.CellSelect && shift && !ctrl:
		g.extendCell(ri, col) // cell rectangle anchor..clicked cell
	case g.MultiSelect && shift:
		g.rangeSelect(ri, ctrl) // Ctrl+Shift adds the range to what is already selected
		g.anchorCol = col
	case g.MultiSelect && ctrl:
		g.toggleRow(ri)
		g.anchorCol = col
	default:
		g.collapseTo(ri)
		g.anchorCol = col
	}
	g.selected = ri
	g.curCol = col
	g.focusGrid()
	g.redraw()
	if g.OnSelected != nil && prev != g.selected {
		g.OnSelected(g.selected, g.rowOf(g.selected))
	}
	if prev != g.selected || wasMulti || g.selOn {
		g.fireSelection()
	}
}

// selectAt selects the body cell under (x, y) and gives the grid keyboard focus.
//
// Fyne reports a click during which the pointer moved even a pixel or two
// (mouse jitter, touchpad, touchscreen) as a drag instead of a tap, and then
// never calls Tapped. Without this the click would neither move the cursor nor
// take focus, so the cursor stayed on the old cell (or the old editor stayed
// open) and the next Enter moved on from there.
func (g *DataGrid) selectAt(x, y float32) {
	if g.rowH <= 0 || y < g.headerH {
		return
	}
	vr := g.topRow + int((y-g.headerH)/g.rowH)
	if col := g.colAt(x); vr < len(g.view) && col >= 0 {
		g.clickCell(vr, col)
	}
}

// activate = double click: start editing if possible, then fire OnActivated.
func (g *DataGrid) activate() {
	if g.selected < 0 {
		return
	}
	if g.edit == nil && g.cursorEditable() {
		g.beginEdit("", false)
	}
	if g.OnActivated != nil {
		g.OnActivated(g.selected, g.rowOf(g.selected))
	}
}

func (g *DataGrid) focusGrid() {
	if c := fyne.CurrentApp().Driver().CanvasForObject(g); c != nil {
		c.Focus(g)
	}
}

// ---------------------------------------------------------------------------
// Focus + keyboard navigation
// ---------------------------------------------------------------------------

// Shift is tracked from KeyDown / KeyUp, which only reach the focused widget:
// a Shift release that happens while another widget (the editor, the next
// field) has focus is never seen here. So the flag is cleared on every focus
// change instead of being left stuck on.
func (g *DataGrid) FocusGained() { g.focused, g.shift = true, false; g.redraw() }
func (g *DataGrid) FocusLost()   { g.focused, g.shift = false, false; g.redraw() }

// AcceptsTab makes Tab/Shift+Tab move between cells; at the first/last cell
// focus is released to the next/previous widget in the form.
func (g *DataGrid) AcceptsTab() bool { return true }

func (g *DataGrid) KeyDown(k *fyne.KeyEvent) {
	if k.Name == desktop.KeyShiftLeft || k.Name == desktop.KeyShiftRight {
		g.shift = true
	}
}

func (g *DataGrid) KeyUp(k *fyne.KeyEvent) {
	if k.Name == desktop.KeyShiftLeft || k.Name == desktop.KeyShiftRight {
		g.shift = false
	}
}

// resizeCursorCol widens (delta > 0) or narrows (delta < 0) the cursor column
// by delta px, like dragging its header edge. Does nothing unless Resizable.
func (g *DataGrid) resizeCursorCol(delta float32) {
	c := g.curCol
	if !g.Resizable || c < 0 || c >= len(g.cols) || c >= len(g.userW) || c >= len(g.colW) {
		return
	}
	w := fyne.Max(g.colW[c]+delta, minUserColW)
	if w == g.userW[c] || w == g.colW[c] {
		return
	}
	g.userW[c] = w
	g.redraw() // recomputes colW / colX
	g.ensureColVisible(c)
	g.redraw()
	if g.OnColumnResized != nil {
		g.OnColumnResized(c, w)
	}
}

// Typing on a selected editable cell starts editing and replaces its content.
func (g *DataGrid) TypedRune(r rune) {
	if g.edit != nil || !g.cursorEditable() || g.cursorHasWidget() {
		return
	}
	g.beginEdit(string(r), true)
}

func (g *DataGrid) TypedKey(k *fyne.KeyEvent) {
	if g.edit != nil {
		return // the editor handles its own keys
	}
	if g.OnKey != nil {
		idx := -1
		var rw Row
		if g.selected >= 0 && g.selected < g.src.Len() {
			idx, rw = g.selected, g.rowOf(g.selected)
		}
		if g.OnKey(k, idx, g.ColumnKey(g.curCol), rw) {
			return
		}
	}
	row := g.cursorViewRow()
	page := maxInt(g.visRows-1, 1)
	switch k.Name {
	case fyne.KeyUp:
		g.moveByExt(-1, 0, g.shift)
	case fyne.KeyDown:
		g.moveByExt(1, 0, g.shift)
	case fyne.KeyLeft:
		g.moveByExt(0, -1, g.shift && g.CellSelect)
	case fyne.KeyRight:
		g.moveByExt(0, 1, g.shift && g.CellSelect)
	case fyne.KeyPageUp:
		g.moveByExt(-page, 0, g.shift)
	case fyne.KeyPageDown:
		g.moveByExt(page, 0, g.shift)
	case fyne.KeyEscape:
		g.collapseToCursor()
	case fyne.KeyHome:
		g.moveToExt(maxInt(row, 0), 0, g.shift && g.CellSelect)
	case fyne.KeyEnd:
		g.moveToExt(maxInt(row, 0), len(g.cols)-1, g.shift && g.CellSelect)
	case fyne.KeyTab:
		g.tab(g.shift)
	case fyne.KeyF2:
		g.beginEdit("", false)
	case fyne.KeyReturn, fyne.KeyEnter:
		if g.fireEnter() {
			return
		}
		if g.EnterMovesNext {
			g.enterNext(g.shift)
		}
	case fyne.KeyDelete, fyne.KeyBackspace:
		g.clearCell()
	}
}

func (g *DataGrid) cursorViewRow() int {
	if g.selected < 0 || g.selected >= len(g.pos) {
		return -1
	}
	return g.pos[g.selected]
}

func (g *DataGrid) moveTo(vr, col int) { g.moveToExt(vr, col, false) }

// moveToExt is moveTo. With extend (Shift held) the selection grows from the
// anchor row to the new cursor row instead of collapsing onto it.
func (g *DataGrid) moveToExt(vr, col int, extend bool) {
	if len(g.view) == 0 || len(g.cols) == 0 {
		return
	}
	vr = clampInt(vr, 0, len(g.view)-1)
	col = clampInt(col, 0, len(g.cols)-1)
	prev, wasMulti := g.selected, g.selOn
	ri := g.view[vr]
	switch {
	case extend && g.CellSelect:
		g.extendCell(ri, col) // reads the anchor / old cursor, so it runs before g.selected changes
	case extend && g.MultiSelect:
		g.rangeSelect(ri, false) // reads the anchor / old cursor, so it runs before g.selected changes
	default:
		g.collapseTo(ri)
		g.anchorCol = col
	}
	g.selected = ri
	g.curCol = col
	g.ensureVisible(vr)
	g.ensureColVisible(col)
	g.redraw()
	if g.OnSelected != nil && prev != g.selected {
		g.OnSelected(g.selected, g.rowOf(g.selected))
	}
	if prev != g.selected || wasMulti || g.selOn {
		g.fireSelection()
	}
}

func (g *DataGrid) moveBy(dr, dc int) { g.moveByExt(dr, dc, false) }

func (g *DataGrid) moveByExt(dr, dc int, extend bool) {
	vr := g.cursorViewRow()
	if vr < 0 {
		g.moveTo(0, 0)
		return
	}
	g.moveToExt(vr+dr, g.curCol+dc, extend)
}

// step moves the cursor to the next cell (or the previous one when back is
// true): along the row, and past the last column to the first column of the
// next row. It reports false, without moving, at the very first / last cell.
func (g *DataGrid) step(back bool) bool {
	vr := g.cursorViewRow()
	if vr < 0 {
		g.moveTo(0, 0)
		return true
	}
	last := len(g.cols) - 1
	switch {
	case !back && g.curCol < last:
		g.moveTo(vr, g.curCol+1)
	case !back && vr < len(g.view)-1:
		g.moveTo(vr+1, 0)
	case back && g.curCol > 0:
		g.moveTo(vr, g.curCol-1)
	case back && vr > 0:
		g.moveTo(vr-1, last)
	default:
		return false
	}
	return true
}

// entryRange returns the first / last column Enter walks through, from
// EnterFirstCol / EnterLastCol. Unknown keys, or a first column that lies after
// the last one, fall back to the whole row.
func (g *DataGrid) entryRange() (first, last int) {
	first, last = 0, len(g.cols)-1
	f, l := first, last
	if g.EnterFirstCol != "" {
		if i := g.colIndex(g.EnterFirstCol); i >= 0 {
			f = i
		}
	}
	if g.EnterLastCol != "" {
		if i := g.colIndex(g.EnterLastCol); i >= 0 {
			l = i
		}
	}
	if f <= l {
		first, last = f, l
	}
	return first, last
}

// enterNext is what Enter does (Shift+Enter: back = true): next cell inside
// the Enter range, then the first cell of the next row, and after the last
// cell of the last row a new row (see AutoAddRow).
func (g *DataGrid) enterNext(back bool) {
	g.ensureView()
	first, last := g.entryRange()
	vr := g.cursorViewRow()
	if vr < 0 {
		g.moveTo(0, first)
		return
	}
	c := g.curCol
	if !back {
		switch {
		case c < first:
			g.moveTo(vr, first)
		case c < last:
			g.moveTo(vr, c+1)
		case vr < len(g.view)-1:
			g.moveTo(vr+1, first)
		default:
			g.addRowAndMove(first)
		}
		return
	}
	switch {
	case c > last:
		g.moveTo(vr, last)
	case c > first:
		g.moveTo(vr, c-1)
	case vr > 0:
		g.moveTo(vr-1, last)
	}
}

// addRowAndMove adds a row (AutoAddRow) and puts the cursor in column col of it.
func (g *DataGrid) addRowAndMove(col int) {
	if !g.AutoAddRow || g.ReadOnly || !g.editable() {
		return
	}
	if g.AddRow == nil {
		return
	}
	before := g.src.Len()
	if err := g.AddRow(); err != nil {
		return
	}
	g.Refresh() // pick up the new length and rebuild the display order
	n := g.src.Len()
	if n <= before {
		return
	}
	g.moveTo(g.pos[n-1], col) // honours an active sort: the new row may not be last on screen
	if g.OnRowAdded != nil {
		g.OnRowAdded(n-1, g.rowOf(n-1))
	}
}

// tab is step, but at the first / last cell focus moves to the previous /
// next widget in the form. (Enter uses step directly and stays in the grid.)
func (g *DataGrid) tab(back bool) {
	if g.step(back) {
		return
	}
	if c := fyne.CurrentApp().Driver().CanvasForObject(g); c != nil {
		if back {
			c.FocusPrevious()
		} else {
			c.FocusNext()
		}
	}
}

// ---------------------------------------------------------------------------
// Editing
// ---------------------------------------------------------------------------

func (g *DataGrid) isEditable(col int) bool {
	return !g.ReadOnly && g.editable() && col >= 0 && col < len(g.cols) && !g.cols[col].ReadOnly
}

func (g *DataGrid) cursorEditable() bool {
	return g.cursorViewRow() >= 0 && g.isEditable(g.curCol)
}

func (g *DataGrid) beginEdit(initial string, replace bool) {
	if g.edit != nil || !g.cursorEditable() || g.cursorHasWidget() {
		return
	}
	vr := g.cursorViewRow()
	ri := g.view[vr]
	col := g.cols[g.curCol]
	old := g.src.Value(ri, col.Key)
	text := col.editText(old)
	if replace {
		text = initial
	}
	g.edit = &editState{row: ri, col: g.curCol, old: old, text: text}

	g.quiet = true
	g.entry.SetText(text)
	g.quiet = false

	g.ensureVisible(vr)
	g.ensureColVisible(g.curCol)
	g.redraw() // the renderer positions + shows the entry
	g.focusEditor()
}

func (g *DataGrid) focusEditor() {
	if g.edit == nil {
		return
	}
	cv := fyne.CurrentApp().Driver().CanvasForObject(g)
	if cv == nil {
		return
	}
	e := g.entry
	cv.Focus(e)
	e.CursorRow = 0
	e.CursorColumn = len([]rune(e.Text))
	e.Refresh()
}

// endEdit closes the editor without applying anything.
func (g *DataGrid) endEdit(refocus bool) {
	g.edit = nil
	g.redraw()
	if refocus {
		g.focusGrid()
	}
}

// commitEdit parses + validates the editor text and applies it.
// On failure the editor stays open (showing the error) and false is returned.
func (g *DataGrid) commitEdit() bool {
	ed := g.edit
	if ed == nil {
		return true
	}
	col := g.cols[ed.col]
	val, err := col.parse(ed.text)
	var ev CellChange
	var changed bool
	if err == nil {
		ev, changed, err = g.applyChange(ed.row, ed.col, ed.old, val)
	}
	if err != nil {
		g.entry.SetValidationError(err)
		return false
	}
	g.edit = nil
	g.redraw()
	if changed && g.OnCellChanged != nil {
		g.OnCellChanged(ev)
	}
	return true
}

// applyChange runs BeforeCellChange, then stores the value in the source.
func (g *DataGrid) applyChange(ri, ci int, old, val any) (CellChange, bool, error) {
	col := g.cols[ci]
	es, ok := g.src.(EditableSource)
	if !ok {
		return CellChange{}, false, fmt.Errorf("this data source is read-only")
	}
	ev := CellChange{RowIndex: ri, Col: ci, Key: col.Key, Old: old, New: val, Row: g.rowOf(ri)}
	if g.BeforeCellChange != nil {
		if err := g.BeforeCellChange(ev); err != nil {
			return ev, false, err
		}
	}
	if col.sameValue(old, val) {
		return ev, false, nil
	}
	if err := es.SetValue(ri, col.Key, val); err != nil {
		return ev, false, err
	}
	ev.Row = g.rowOf(ri) // now holds the new value
	return ev, true, nil
}

// Delete / Backspace on a selected cell (or cell rectangle) clears it.
func (g *DataGrid) clearCell() {
	if g.cellOn {
		g.clearCellRange()
		return
	}
	if !g.cursorEditable() {
		return
	}
	ri := g.selected
	col := g.cols[g.curCol]
	val, _ := col.parse("")
	ev, changed, err := g.applyChange(ri, g.curCol, g.src.Value(ri, col.Key), val)
	if err != nil {
		return
	}
	g.redraw()
	if changed && g.OnCellChanged != nil {
		g.OnCellChanged(ev)
	}
}

// Called by the cell editor for Enter / Esc / Tab / Up / Down.
func (g *DataGrid) onEditorNav(key fyne.KeyName, shift bool) {
	if g.edit == nil {
		return
	}
	if key == fyne.KeyEscape {
		g.endEdit(true)
		return
	}
	if !g.commitEdit() {
		return // rejected: stay in the editor
	}
	g.focusGrid()
	switch key {
	case fyne.KeyReturn:
		if g.fireEnter() {
			return
		}
		if g.EnterMovesNext {
			g.enterNext(shift)
		}
	case fyne.KeyDown:
		g.moveBy(1, 0)
	case fyne.KeyUp:
		g.moveBy(-1, 0)
	case fyne.KeyTab:
		g.tab(shift)
	}
}

// Called when the editor loses focus (user clicked elsewhere).
func (g *DataGrid) onEditorBlur() {
	if g.edit == nil {
		return
	}
	if !g.commitEdit() {
		g.endEdit(false) // invalid value + focus gone: revert
	}
}

// ---------------------------------------------------------------------------
// Renderer: draws the visible window of rows with a fixed pool of canvas
// objects. Nothing here allocates per row of data; only per visible row.
// ---------------------------------------------------------------------------

// fitCache remembers the last truncation result of one text object so text
// is only measured when its content, width or the theme changed.
type fitCache struct {
	valid bool
	gen   uint32
	w     float32
	size  float32
	style fyne.TextStyle
	src   string
	out   string
}

// fit shortens s with an ellipsis so it renders within width w.
func (g *DataGrid) fit(c *fitCache, s string, w float32, style fyne.TextStyle) string {
	return g.fitSized(c, s, w, g.textSize, style)
}

// fitSized is fit with an explicit text size (for styled cells).
func (g *DataGrid) fitSized(c *fitCache, s string, w, size float32, style fyne.TextStyle) string {
	if c.valid && c.gen == g.themeGen && c.w == w && c.size == size && c.style == style && c.src == s {
		return c.out
	}
	out := s
	switch {
	case s == "" || w <= 0:
		out = ""
	case fyne.MeasureText(s, size, style).Width > w:
		rs := []rune(s)
		lo, hi := 0, len(rs) // largest n such that rs[:n]+"…" fits
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if fyne.MeasureText(string(rs[:mid])+"…", size, style).Width <= w {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		out = string(rs[:lo]) + "…"
	}
	*c = fitCache{valid: true, gen: g.themeGen, w: w, size: size, style: style, src: s, out: out}
	return out
}

type dataGridRenderer struct {
	g *DataGrid

	rowBg []*canvas.Rectangle // one background per visible row
	rowSt []int8              // last background state per row (-1 = unknown)
	cells [][]*canvas.Text    // [visible row][col]
	cache [][]fitCache
	cellBg [][]*canvas.Rectangle // [visible row][col] custom cell backgrounds (hidden unless styled)
	cellW  [][]fyne.CanvasObject // [visible row][col] widget from DataGrid.CellWidget (nil = none)

	hdrBg    *canvas.Rectangle
	hdrLine  *canvas.Rectangle
	hdrTxt   []*canvas.Text
	hdrCache []fitCache
	vlines   []*canvas.Rectangle // column separators (full height)

	cursor *canvas.Rectangle // cursor cell outline
	range_ *canvas.Rectangle // selected cell rectangle (drawn under the cell texts)
	thumb  *canvas.Rectangle // vertical scrollbar thumb
	hthumb *canvas.Rectangle // horizontal scrollbar thumb
	thumbOK, hthumbOK bool   // thumb has been styled since the last theme change

	objects []fyne.CanvasObject
	seenGen uint32
	colGen  uint32
}

var boldStyle = fyne.TextStyle{Bold: true}

func newDataGridRenderer(g *DataGrid) *dataGridRenderer {
	r := &dataGridRenderer{
		g:       g,
		hdrBg:   canvas.NewRectangle(color.Transparent),
		hdrLine: canvas.NewRectangle(color.Transparent),
		cursor:  canvas.NewRectangle(color.Transparent),
		range_:  canvas.NewRectangle(color.Transparent),
		thumb:   canvas.NewRectangle(color.Transparent),
		hthumb:  canvas.NewRectangle(color.Transparent),
	}
	r.setupColumns()
	g.rend = r
	return r
}

// setupColumns (re)creates everything that depends on the column list.
func (r *dataGridRenderer) setupColumns() {
	g := r.g
	r.colGen = g.colsGen
	r.rowBg, r.rowSt, r.cells, r.cache, r.cellBg, r.cellW = nil, nil, nil, nil, nil, nil
	r.hdrTxt = make([]*canvas.Text, len(g.cols))
	r.hdrCache = make([]fitCache, len(g.cols))
	r.vlines = nil
	for i, c := range g.cols {
		t := canvas.NewText("", color.Transparent)
		t.TextSize = 0 // 0 = not styled yet, see setText
		t.TextStyle = boldStyle
		t.Alignment = c.Align
		r.hdrTxt[i] = t
	}
	for i := 0; i < len(g.cols)-1; i++ {
		r.vlines = append(r.vlines, canvas.NewRectangle(color.Transparent))
	}
	r.seenGen = ^uint32(0) // force a full style pass
	r.rebuild()
}

// rebuild recreates the draw-order list. Order = z-order (later is on top).
func (r *dataGridRenderer) rebuild() {
	o := make([]fyne.CanvasObject, 0, 8+len(r.rowBg)*(2+2*len(r.g.cols)))
	for _, b := range r.rowBg {
		o = append(o, b)
	}
	for _, row := range r.cellBg {
		for _, b := range row {
			o = append(o, b)
		}
	}
	o = append(o, r.range_, r.hdrBg, r.hdrLine)
	for _, v := range r.vlines {
		o = append(o, v)
	}
	for _, row := range r.cells {
		for _, t := range row {
			o = append(o, t)
		}
	}
	for _, row := range r.cellW {
		for _, w := range row {
			if w != nil {
				o = append(o, w)
			}
		}
	}
	for _, t := range r.hdrTxt {
		o = append(o, t)
	}
	o = append(o, r.cursor, r.g.entry, r.thumb, r.hthumb)
	r.objects = o
}

// ensureRows grows the pool to n visible rows. Reports whether it grew.
func (r *dataGridRenderer) ensureRows(n int) bool {
	if len(r.rowBg) >= n {
		return false
	}
	for len(r.rowBg) < n {
		r.rowBg = append(r.rowBg, canvas.NewRectangle(color.Transparent))
		r.rowSt = append(r.rowSt, -1)
		row := make([]*canvas.Text, len(r.g.cols))
		for i, c := range r.g.cols {
			t := canvas.NewText("", color.Transparent)
			t.TextSize = 0 // 0 = not styled yet, see setText
			t.Alignment = c.Align
			row[i] = t
		}
		r.cells = append(r.cells, row)
		r.cache = append(r.cache, make([]fitCache, len(r.g.cols)))
		bgs := make([]*canvas.Rectangle, len(r.g.cols))
		for i := range bgs {
			bgs[i] = canvas.NewRectangle(color.Transparent)
			bgs[i].Hide()
		}
		r.cellBg = append(r.cellBg, bgs)
		r.cellW = append(r.cellW, make([]fyne.CanvasObject, len(r.g.cols)))
	}
	r.rebuild()
	return true
}

func (r *dataGridRenderer) Objects() []fyne.CanvasObject { return r.objects }
func (r *dataGridRenderer) Destroy()                     {}

func (r *dataGridRenderer) MinSize() fyne.Size {
	g := r.g
	h := g.rowH
	if h <= 0 {
		h = 30
	}
	return fyne.NewSize(120, h*4) // small on purpose: wide column sets scroll sideways
}

func (r *dataGridRenderer) Layout(size fyne.Size) { r.sync(size) }

func (r *dataGridRenderer) Refresh() {
	r.sync(r.g.Size())
	r.range_.Refresh()
	r.cursor.Refresh() // make sure the canvas repaints even if nothing else changed
}

// place moves/resizes only when something actually changed.
func place(o fyne.CanvasObject, x, y, w, h float32) {
	if p := fyne.NewPos(x, y); o.Position() != p {
		o.Move(p)
	}
	if s := fyne.NewSize(w, h); o.Size() != s {
		o.Resize(s)
	}
}

func setVisible(o fyne.CanvasObject, v bool) {
	if o.Visible() != v {
		if v {
			o.Show()
		} else {
			o.Hide()
		}
	}
}

// setText updates a text object, refreshing it only if it actually changed.
func (r *dataGridRenderer) setText(t *canvas.Text, s string, restyle bool) {
	style := restyle || t.TextSize == 0 // never styled: it was hidden during the last restyle
	changed := style
	if t.Text != s {
		t.Text = s
		changed = true
	}
	if style {
		t.Color = r.g.colFg
		t.TextSize = r.g.textSize
	}
	if changed {
		t.Refresh()
	}
}

func (r *dataGridRenderer) sync(size fyne.Size) {
	g := r.g
	if r.colGen != g.colsGen {
		r.setupColumns()
	}
	g.layoutMetrics(size)
	grew := r.ensureRows(g.visRows)
	restyle := grew || r.seenGen != g.themeGen
	r.seenGen = g.themeGen

	r.syncHeader(size, restyle)
	r.syncRows(size, restyle)
	r.syncCursorAndEditor()
	r.syncBar(size, restyle)
}

func (r *dataGridRenderer) syncHeader(size fyne.Size, restyle bool) {
	g := r.g
	pad := theme.InnerPadding()

	place(r.hdrBg, 0, 0, size.Width, g.headerH)
	place(r.hdrLine, 0, g.headerH-1, size.Width, 1)
	if restyle {
		r.hdrBg.FillColor = g.colHeader
		r.hdrBg.Refresh()
		r.hdrLine.FillColor = g.colSep
		r.hdrLine.Refresh()
	}
	for i, v := range r.vlines {
		x := g.colX[i] + g.colW[i]
		place(v, x, 0, 1, size.Height-g.sbH)
		setVisible(v, x > 0 && x < g.viewW)
		if restyle {
			v.FillColor = g.colSep
			v.Refresh()
		}
	}
	for i, t := range r.hdrTxt {
		x0, x1, ok := g.colVisible(i)
		w := x1 - x0 - 2*pad
		if !ok || w <= 0 { // scrolled out of view
			setVisible(t, false)
			continue
		}
		title := g.cols[i].Title
		if g.sortCol == i {
			if g.sortAsc {
				title += " ↑"
			} else {
				title += " ↓"
			}
		}
		setVisible(t, true)
		r.setText(t, g.fit(&r.hdrCache[i], title, w, boldStyle), restyle)
		place(t, x0+pad, 0, w, g.headerH)
	}
}

func (r *dataGridRenderer) syncRows(size fyne.Size, restyle bool) {
	bodyW := size.Width - r.g.sbW
	for k := range r.rowBg {
		r.syncRow(k, bodyW, restyle)
	}
}

// syncRow brings one pooled row (background + all its cells) up to date.
func (r *dataGridRenderer) syncRow(k int, bodyW float32, restyle bool) {
	g := r.g
	bg := r.rowBg[k]
	vr := g.topRow + k
	if vr >= len(g.view) { // pool row not needed right now
		setVisible(bg, false)
		r.rowSt[k] = -1
		for _, t := range r.cells[k] {
			setVisible(t, false)
		}
		for _, b := range r.cellBg[k] {
			setVisible(b, false)
		}
		for _, w := range r.cellW[k] {
			if w != nil {
				setVisible(w, false)
			}
		}
		return
	}
	ri := g.view[vr]
	y := g.headerH + float32(k)*g.rowH

	var st int8
	var fill color.Color = color.Transparent
	switch {
	case g.rowSelected(ri):
		st, fill = 1, g.colSel
	case g.Striped && vr%2 == 1:
		st, fill = 2, g.colStripe
	}
	place(bg, 0, y, bodyW, g.rowH)
	setVisible(bg, true)
	if restyle || r.rowSt[k] != st {
		r.rowSt[k] = st
		bg.FillColor = fill
		bg.Refresh()
	}

	for c := range r.cells[k] {
		r.syncCell(k, c, ri, y, restyle)
	}
}

// syncCell brings one pooled cell up to date. k is the pool row, ri the
// source row index shown there, y its top edge.
func (r *dataGridRenderer) syncCell(k, c, ri int, y float32, restyle bool) {
	g := r.g
	t := r.cells[k][c]
	pad := theme.InnerPadding()
	x0, x1, ok := g.colVisible(c)
	w := x1 - x0 - 2*pad
	bg := r.cellBg[k][c]
	if !ok || w <= 0 { // column scrolled out of view: don't even read its data
		setVisible(t, false)
		setVisible(bg, false)
		if cw := r.cellW[k][c]; cw != nil {
			setVisible(cw, false)
		}
		return
	}
	st := g.styleOf(ri, c)
	size := g.textSize
	if st.Size > 0 {
		size = st.Size
	}
	s := g.fitSized(&r.cache[k][c], g.cellText(ri, g.cols[c]), w, size, st.textStyle())
	setVisible(t, true)
	r.setText(t, s, restyle)
	// styling is applied every time: pooled objects are reused by other cells
	fg := g.colFg
	if st.Color != nil {
		fg = st.Color
	}
	if !sameColor(t.Color, fg) || t.TextSize != size || t.TextStyle != st.textStyle() {
		t.Color, t.TextSize, t.TextStyle = fg, size, st.textStyle()
		t.Refresh()
	}
	if st.Background != nil {
		place(bg, x0, y, x1-x0, g.rowH)
		if !sameColor(bg.FillColor, st.Background) {
			bg.FillColor = st.Background
			bg.Refresh()
		}
		setVisible(bg, true)
	} else {
		setVisible(bg, false)
	}
	place(t, x0+pad, y, w, g.rowH)
	r.syncCellWidget(k, c, ri, x0, x1, y)
}

// syncCellWidget asks CellWidget for this cell's widget, swaps it in when it
// changed, and places it over the cell (the text is hidden while it shows).
func (r *dataGridRenderer) syncCellWidget(k, c, ri int, x0, x1, y float32) {
	g := r.g
	if g.CellWidget == nil {
		if r.cellW[k][c] != nil {
			r.cellW[k][c] = nil
			r.rebuild()
		}
		return
	}
	old := r.cellW[k][c]
	nw := g.CellWidget(ri, g.cols[c].Key, old)
	if nw != old {
		r.cellW[k][c] = nw
		r.rebuild() // draw-order list changed
	}
	if nw == nil {
		return
	}
	setVisible(r.cells[k][c], false) // widget replaces the text
	place(nw, x0, y, x1-x0, g.rowH)
	setVisible(nw, true)
	nw.Refresh()
}

// syncRange places the selected cell rectangle (clipped to what is on screen).
func (r *dataGridRenderer) syncRange() {
	g := r.g
	show := false
	if v1, v2, c1, c2, ok := g.cellRect(); ok && len(r.rowBg) > 0 {
		k1 := maxInt(v1-g.topRow, 0)
		k2 := minInt(v2-g.topRow, len(r.rowBg)-1)
		var xa, xb float32
		found := false
		for c := c1; c <= c2 && c < len(g.colX); c++ {
			if a, b, vis := g.colVisible(c); vis {
				if !found {
					xa, found = a, true
				}
				xb = b
			}
		}
		if found && k1 <= k2 {
			place(r.range_, xa, g.headerH+float32(k1)*g.rowH, xb-xa, float32(k2-k1+1)*g.rowH)
			r.range_.FillColor = g.colSel
			r.range_.StrokeColor = g.colPrimary
			r.range_.StrokeWidth = 1
			show = true
		}
	}
	setVisible(r.range_, show)
}

func (r *dataGridRenderer) syncCursorAndEditor() {
	g := r.g
	r.syncRange()

	// cursor outline (clipped to the visible part of its column)
	vr := g.cursorViewRow()
	k := vr - g.topRow
	showCursor := false
	if vr >= 0 && k >= 0 && k < len(r.rowBg) && g.curCol < len(g.colX) {
		if x0, x1, ok := g.colVisible(g.curCol); ok {
			place(r.cursor, x0, g.headerH+float32(k)*g.rowH, x1-x0, g.rowH)
			r.cursor.FillColor = color.Transparent
			r.cursor.StrokeColor = g.colPrimary
			r.cursor.StrokeWidth = 1
			if g.focused || g.edit != nil {
				r.cursor.StrokeWidth = 2
			}
			showCursor = true
		}
	}
	setVisible(r.cursor, showCursor)

	// inline editor (hidden while its row or column is scrolled out of view)
	showEditor := false
	if ed := g.edit; ed != nil && ed.row < len(g.pos) && ed.col < len(g.colX) {
		ek := g.pos[ed.row] - g.topRow
		if ek >= 0 && ek < len(r.rowBg) {
			if x0, x1, ok := g.colVisible(ed.col); ok {
				place(g.entry, x0, g.headerH+float32(ek)*g.rowH, x1-x0, g.rowH)
				showEditor = true
			}
		}
	}
	setVisible(g.entry, showEditor)
}

func (r *dataGridRenderer) syncBar(size fyne.Size, restyle bool) {
	g := r.g
	if restyle {
		r.thumbOK, r.hthumbOK = false, false
	}

	// vertical thumb
	if g.hasBar {
		track := size.Height - g.headerH - g.sbH
		total := len(g.view)
		g.thumbH = fyne.Max(24, track*float32(g.visRows)/float32(total))
		g.thumbY = g.headerH
		if maxTop := total - g.visRows; maxTop > 0 {
			g.thumbY += (track - g.thumbH) * float32(g.topRow) / float32(maxTop)
		}
		w := g.sbW - 4
		place(r.thumb, g.viewW+2, g.thumbY, w, g.thumbH)
		if !r.thumbOK {
			r.thumbOK = true
			r.thumb.FillColor = g.colBar
			r.thumb.CornerRadius = w / 2
			r.thumb.Refresh()
		}
		setVisible(r.thumb, true)
	} else {
		setVisible(r.thumb, false)
	}

	// horizontal thumb
	if g.hasH {
		track := g.viewW
		g.hthumbW = fyne.Max(24, track*track/g.contentW)
		g.hthumbX = 0
		if maxLeft := g.contentW - g.viewW; maxLeft > 0 {
			g.hthumbX = (track - g.hthumbW) * g.leftX / maxLeft
		}
		h := g.sbH - 4
		place(r.hthumb, g.hthumbX, size.Height-g.sbH+2, g.hthumbW, h)
		if !r.hthumbOK {
			r.hthumbOK = true
			r.hthumb.FillColor = g.colBar
			r.hthumb.CornerRadius = h / 2
			r.hthumb.Refresh()
		}
		setVisible(r.hthumb, true)
	} else {
		setVisible(r.hthumb, false)
	}
}

// ---------------------------------------------------------------------------
// Inline editor entry: Enter / Esc / Tab / Up / Down go to the grid
// ---------------------------------------------------------------------------

type cellEntry struct {
	widget.Entry
	nav   func(key fyne.KeyName, shift bool)
	blur  func()
	shift bool
}

func newCellEntry() *cellEntry {
	e := &cellEntry{}
	e.ExtendBaseWidget(e)
	return e
}

func (e *cellEntry) AcceptsTab() bool { return true }

func (e *cellEntry) KeyDown(k *fyne.KeyEvent) {
	if k.Name == desktop.KeyShiftLeft || k.Name == desktop.KeyShiftRight {
		e.shift = true
	}
	e.Entry.KeyDown(k)
}

func (e *cellEntry) KeyUp(k *fyne.KeyEvent) {
	if k.Name == desktop.KeyShiftLeft || k.Name == desktop.KeyShiftRight {
		e.shift = false
	}
	e.Entry.KeyUp(k)
}

func (e *cellEntry) TypedKey(k *fyne.KeyEvent) {
	if e.nav != nil {
		switch k.Name {
		case fyne.KeyReturn, fyne.KeyEnter:
			e.nav(fyne.KeyReturn, e.shift)
			return
		case fyne.KeyEscape, fyne.KeyTab, fyne.KeyUp, fyne.KeyDown:
			e.nav(k.Name, e.shift)
			return
		}
	}
	e.Entry.TypedKey(k)
}

func (e *cellEntry) FocusGained() {
	e.shift = false // a Shift press before focus arrived was not seen
	e.Entry.FocusGained()
}

func (e *cellEntry) FocusLost() {
	e.shift = false // its release will go to whoever has focus next
	e.Entry.FocusLost()
	if e.blur != nil {
		e.blur()
	}
}

// ---------------------------------------------------------------------------
// Value helpers
// ---------------------------------------------------------------------------

// cellText is the text shown for one cell: FormatCell first, then the column.
func (g *DataGrid) cellText(ri int, col Column) string {
	if g.FormatCell != nil {
		if s, ok := g.FormatCell(ri, col.Key); ok {
			return s
		}
	}
	return col.format(g.src.Value(ri, col.Key))
}

func (c Column) format(v any) string {
	if c.Formatter != nil {
		return c.Formatter(v)
	}
	if v == nil {
		return ""
	}
	switch c.Type {
	case ColNumber:
		d, ok := toDecimal(v)
		if !ok || (c.HideZero && d.IsZero()) {
			return ""
		}
		s := d.StringFixed(int32(c.Decimals))
		if c.Thousands {
			s = AddThousands(s)
		}
		return s
	case ColInt:
		n, ok := toInt64(v)
		if !ok {
			return ""
		}
		s := strconv.FormatInt(n, 10)
		if c.Thousands {
			s = AddThousands(s)
		}
		return s
	case ColDate:
		t, ok := toTime(v)
		if !ok || t.IsZero() {
			return ""
		}
		l := c.DateLayout
		if l == "" {
			l = dateLayout
		}
		return t.Format(l)
	default:
		return fmt.Sprint(v)
	}
}

// editText is what the editor starts with (numbers unrounded, no padding).
func (c Column) editText(v any) string {
	if c.Type == ColNumber {
		d, ok := toDecimal(v)
		if !ok || (d.IsZero() && c.HideZero) {
			return ""
		}
		return d.String()
	}
	c.Formatter = nil // c is a copy: the editor must show the raw value, not display text
	return c.format(v)
}

// parse converts editor text into a typed value.
func (c Column) parse(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch c.Type {
	case ColNumber:
		if s == "" {
			return decimal.Zero, nil
		}
		d, err := decimal.NewFromString(strings.ReplaceAll(s, ",", ""))
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid number", s)
		}
		return d, nil
	case ColInt:
		if s == "" {
			return int64(0), nil
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid whole number", s)
		}
		return n, nil
	case ColDate:
		if s == "" {
			return time.Time{}, nil
		}
		for _, l := range []string{c.DateLayout, dateLayout, "02/01/2006", "02-01-2006", "2/1/2006"} {
			if l == "" {
				continue
			}
			if t, err := time.Parse(l, s); err == nil {
				return t, nil
			}
		}
		return nil, fmt.Errorf("%q is not a valid date", s)
	}
	return s, nil
}

func (c Column) sameValue(a, b any) bool {
	switch c.Type {
	case ColNumber, ColInt, ColDate:
		return compareValues(c.Type, a, b) == 0
	}
	return textOf(a) == textOf(b) // raw values: a Formatter must not decide whether a cell changed
}

// coerce converts a value given from code into the type this column stores,
// so that SetCellValue(row, "Amount", 100) works for a decimal.Decimal field.
func (c Column) coerce(v any) (any, error) {
	switch c.Type {
	case ColNumber:
		if v == nil {
			return decimal.Zero, nil
		}
		if d, ok := toDecimal(v); ok {
			return d, nil
		}
		return nil, fmt.Errorf("%v (%T) is not a valid number for column %q (floats are not accepted, use decimal.NewFromFloat)", v, v, c.Key)
	case ColInt:
		if v == nil {
			return int64(0), nil
		}
		if n, ok := toInt64(v); ok {
			return n, nil
		}
		return nil, fmt.Errorf("%v (%T) is not a whole number for column %q", v, v, c.Key)
	case ColDate:
		switch x := v.(type) {
		case nil:
			return time.Time{}, nil
		case string:
			return c.parse(x) // tries the column layout and the usual ones
		}
		if t, ok := toTime(v); ok {
			return t, nil
		}
		return nil, fmt.Errorf("%v (%T) is not a date for column %q", v, v, c.Key)
	}
	return v, nil
}

// textOf is the plain text of a raw value; nil is "" (not "<nil>").
func textOf(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// intOfKind reads any integer-kinded value, including named types such as
// `type Status int` that the exact-type switches below do not match.
func intOfKind(v any) (int64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if u := rv.Uint(); u <= 1<<63-1 {
			return int64(u), true
		}
	}
	return 0, false
}

// toDecimal converts a cell value to a decimal.Decimal. float types are
// deliberately not accepted: supply decimal.Decimal, an integer or a string.
func toDecimal(v any) (decimal.Decimal, bool) {
	switch x := v.(type) {
	case decimal.Decimal:
		return x, true
	case *decimal.Decimal:
		if x != nil {
			return *x, true
		}
	case int:
		return decimal.NewFromInt(int64(x)), true
	case int32:
		return decimal.NewFromInt32(x), true
	case int64:
		return decimal.NewFromInt(x), true
	case string:
		d, err := decimal.NewFromString(strings.ReplaceAll(strings.TrimSpace(x), ",", ""))
		return d, err == nil
	}
	if n, ok := intOfKind(v); ok {
		return decimal.NewFromInt(n), true
	}
	return decimal.Zero, false
}

// toInt64 converts a cell value to an int64. Floats are deliberately not
// accepted; a decimal.Decimal or string is accepted only if it is a whole number.
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case *int64:
		if x != nil {
			return *x, true
		}
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case decimal.Decimal:
		if x.IsInteger() {
			return x.IntPart(), true
		}
	case string:
		n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(x), ",", ""), 10, 64)
		return n, err == nil
	}
	return intOfKind(v)
}

func toTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case *time.Time:
		if x != nil {
			return *x, true
		}
	case string:
		t, err := time.Parse(dateLayout, x)
		return t, err == nil
	}
	return time.Time{}, false
}

func compareValues(tp ColType, a, b any) int {
	switch tp {
	case ColNumber:
		x, _ := toDecimal(a)
		y, _ := toDecimal(b)
		return x.Cmp(y)
	case ColInt:
		x, _ := toInt64(a)
		y, _ := toInt64(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case ColDate:
		x, _ := toTime(a)
		y, _ := toTime(b)
		switch {
		case x.Before(y):
			return -1
		case x.After(y):
			return 1
		}
		return 0
	}
	return strings.Compare(strings.ToLower(textOf(a)), strings.ToLower(textOf(b)))
}

// AddThousands inserts commas into the integer part of a plain decimal
// string: "-1234567.89" -> "-1,234,567.89".
func AddThousands(s string) string { return groupDigits(s, false) }

// AddThousandsIndian does the same with Indian grouping (last three digits,
// then pairs): "1234567.89" -> "12,34,567.89".
func AddThousandsIndian(s string) string { return groupDigits(s, true) }

func groupDigits(s string, indian bool) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	if len(intPart) <= 3 {
		return sign + intPart + frac
	}
	var groups []string
	if indian {
		groups = append(groups, intPart[len(intPart)-3:])
		rest := intPart[:len(intPart)-3]
		for len(rest) > 2 {
			groups = append([]string{rest[len(rest)-2:]}, groups...)
			rest = rest[:len(rest)-2]
		}
		if rest != "" {
			groups = append([]string{rest}, groups...)
		}
	} else {
		rest := intPart
		for len(rest) > 3 {
			groups = append([]string{rest[len(rest)-3:]}, groups...)
			rest = rest[:len(rest)-3]
		}
		groups = append([]string{rest}, groups...)
	}
	return sign + strings.Join(groups, ",") + frac
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Struct-tag binding: build a whole grid from a struct type.
//
//	type Account struct {
//	    ID     int64           `grid:"title=ID;width=6"`
//	    Ccy    string          `grid:"title=Currency;width=10"`
//	    Name   string          `grid:"title=Name;width=30"`
//	    Amount decimal.Decimal `grid:"title=Amount;width=15;thousands"`
//	    Date   time.Time       `grid:"title=Date;width=10"`
//	    Note   string          // no grid tag: not shown
//	}
//
//	accounts := []Account{...}
//	grid := fynex_datagrid.New(&accounts)
//	grid.ReadOnly = false // optional: makes every column editable
//
// Tag format: every option is a name=value pair, separated by ";"
// (so a title may contain commas). Order does not matter and every option
// is optional. A yes/no option may be written bare ("thousands") or in full
// ("thousands=true", "thousands=false").
//
//	title=Text     header text (default: the field name)
//	width=10       width as a percent of the grid (default 10)
//	key=name       column key (default: the Go field name), used by
//	               RepaintCell, Sum, FormatCell, CellChange.Key ...
//	thousands      thousand separators (numbers and ints)
//	hidezero       blank instead of 0.00 (numbers)
//	decimals=N     decimals shown (numbers, default 2)
//	date=layout    date layout (default 02-Jan-2006)
//	readonly       column cannot be edited
//	align=right    left | right | center
//	min=N          minimum width in px
//
// `grid:""` shows a field with all defaults; `grid:"-"` or no grid tag
// leaves a field out. Supported field types: string, int/int8/int16/int32/
// int64, uint/uint8/uint16/uint32/uint64 (values above MaxInt64 show blank), decimal.Decimal and time.Time. Any other type
// is shown with fmt.Sprint and is read-only.
// ---------------------------------------------------------------------------

var (
	decimalType = reflect.TypeOf(decimal.Decimal{})
	timeType    = reflect.TypeOf(time.Time{})
)

type structField struct {
	index int
}

// New builds a ready-to-use grid from the struct type T (or *T)
// stored in the slice that items points to. Pass a POINTER to the slice so
// that rows you append later are seen by the grid (then call grid.Refresh()).
//
// The grid starts read-only; set grid.ReadOnly = false to allow editing, the
// edit function that writes values back into your structs is already wired.
// It panics if T is not a struct or a grid tag is malformed (a programming
// error, found the first time the program runs).
func New[T any](items *[]T) *DataGrid {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("fynex_datagrid: datagrid.New needs a slice of structs, got %s", t))
	}
	cols, fields := structSchema(t)

	g := NewDataGrid(cols,
		func() int {
			if items == nil {
				return 0
			}
			return len(*items)
		},
		func(row int, key string) any {
			f, ok := fields[key]
			if !ok {
				return nil
			}
			ev, ok := structElem(items, row)
			if !ok {
				return nil
			}
			return ev.Field(f.index).Interface()
		})
	newItem := func() T {
		var item T
		if rv := reflect.ValueOf(&item).Elem(); rv.Kind() == reflect.Ptr {
			rv.Set(reflect.New(rv.Type().Elem())) // []*T: don't insert nil pointers
		}
		return item
	}
	g.AddRow = func() error {
		if items == nil {
			return fmt.Errorf("no slice to add a row to")
		}
		*items = append(*items, newItem())
		return nil
	}
	g.InsertRow = func(at int) error {
		if items == nil {
			return fmt.Errorf("no slice to insert a row into")
		}
		if at < 0 || at > len(*items) {
			return fmt.Errorf("row %d does not exist", at)
		}
		s := append(*items, newItem()) // grow by one (the new item is overwritten below)
		copy(s[at+1:], s[at:])
		s[at] = newItem()
		*items = s
		return nil
	}
	g.DeleteRow = func(at int) error {
		if items == nil || at < 0 || at >= len(*items) {
			return fmt.Errorf("row %d does not exist", at)
		}
		s := *items
		copy(s[at:], s[at+1:])
		var zero T
		s[len(s)-1] = zero // release the pointer / big struct held by the tail slot
		*items = s[:len(s)-1]
		return nil
	}
	g.setEditFunc(func(row int, key string, v any) error {
		f, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown column %q", key)
		}
		ev, ok := structElem(items, row)
		if !ok {
			return fmt.Errorf("row %d does not exist", row)
		}
		return setStructField(ev.Field(f.index), v)
	})
	return g
}

// SelectedItem returns the item under the cursor from the slice you gave to New.
func SelectedItem[T any](g *DataGrid, items []T) (T, bool) {
	var zero T
	i := g.SelectedIndex()
	if i < 0 || i >= len(items) {
		return zero, false
	}
	return items[i], true
}

// SelectedItems returns the items of every selected row, in data order, from
// the slice you gave to New.
func SelectedItems[T any](g *DataGrid, items []T) []T {
	idxs := g.SelectedIndices()
	out := make([]T, 0, len(idxs))
	for _, i := range idxs {
		if i >= 0 && i < len(items) {
			out = append(out, items[i])
		}
	}
	return out
}

// structElem returns the addressable struct at items[row].
func structElem[T any](items *[]T, row int) (reflect.Value, bool) {
	if items == nil || row < 0 || row >= len(*items) {
		return reflect.Value{}, false
	}
	v := reflect.ValueOf(&(*items)[row]).Elem()
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	return v, true
}

func colTypeOf(t reflect.Type) ColType {
	switch {
	case t == decimalType:
		return ColNumber
	case t == timeType:
		return ColDate
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return ColInt
	}
	return ColText
}

// structSchema reads the grid tags of t into columns.
func structSchema(t reflect.Type) ([]Column, map[string]structField) {
	var cols []Column
	fields := map[string]structField{}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag, ok := f.Tag.Lookup("grid")
		if !ok || strings.TrimSpace(tag) == "-" {
			continue
		}
		bad := func(format string, a ...any) {
			panic(fmt.Sprintf("fynex_datagrid: %s.%s: %s", t.Name(), f.Name, fmt.Sprintf(format, a...)))
		}

		// start from the defaults for the field type, then apply the options
		ct := colTypeOf(f.Type)
		var col Column
		switch ct {
		case ColInt:
			col = IntCol(f.Name, f.Name, 10)
		case ColNumber:
			col = NumberCol(f.Name, f.Name, 10)
		case ColDate:
			col = DateCol(f.Name, f.Name, 10)
		default:
			col = TextCol(f.Name, f.Name, 10)
			if f.Type.Kind() != reflect.String {
				col.ReadOnly = true // e.g. bool / float: display only
			}
		}

		for _, o := range strings.Split(tag, ";") {
			o = strings.TrimSpace(o)
			if o == "" {
				continue
			}
			name, val, hasVal := strings.Cut(o, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			val = strings.TrimSpace(val)
			// flag: a bare option means true, otherwise the value must be a bool
			flag := func() bool {
				if !hasVal {
					return true
				}
				b, err := strconv.ParseBool(val)
				if err != nil {
					bad("%s=%q must be true or false", name, val)
				}
				return b
			}
			num := func() float32 {
				v, err := strconv.ParseFloat(val, 32)
				if err != nil || v < 0 {
					bad("%s=%q is not a valid number", name, val)
				}
				return float32(v)
			}
			switch name {
			case "title":
				if val != "" {
					col.Title = val
				}
			case "width":
				col.WidthPct = num()
			case "key":
				if val == "" {
					bad("key= needs a value")
				}
				col.Key = val
			case "thousands":
				col.Thousands = flag()
			case "hidezero":
				col.HideZero = flag()
			case "readonly":
				col.ReadOnly = flag() || col.ReadOnly
			case "decimals":
				col.Decimals = int(num())
			case "date":
				if val == "" {
					bad("date= needs a layout")
				}
				col.DateLayout = val
			case "min":
				col.MinWidth = num()
			case "align":
				switch strings.ToLower(val) {
				case "left":
					col.Align = fyne.TextAlignLeading
				case "right":
					col.Align = fyne.TextAlignTrailing
				case "center":
					col.Align = fyne.TextAlignCenter
				default:
					bad("align=%q must be left, right or center", val)
				}
			default:
				bad("unknown option %q", name)
			}
		}

		if _, dup := fields[col.Key]; dup {
			bad("duplicate column key %q", col.Key)
		}
		fields[col.Key] = structField{index: i}
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		panic(fmt.Sprintf("fynex_datagrid: %s has no fields with a grid tag", t))
	}
	return cols, fields
}

// setStructField stores an edited value (int64, decimal.Decimal, time.Time or
// string, as produced by Column.parse) into a struct field.
func setStructField(fv reflect.Value, v any) error {
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := toInt64(v)
		if !ok {
			return fmt.Errorf("%v is not a whole number", v)
		}
		if fv.OverflowInt(n) {
			return fmt.Errorf("%d is too large for this column", n)
		}
		fv.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, ok := toInt64(v)
		if !ok || n < 0 {
			return fmt.Errorf("%v is not a valid positive whole number", v)
		}
		if fv.OverflowUint(uint64(n)) {
			return fmt.Errorf("%d is too large for this column", n)
		}
		fv.SetUint(uint64(n))
		return nil
	case reflect.String:
		fv.SetString(fmt.Sprint(v))
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.IsValid() && rv.Type().AssignableTo(fv.Type()) {
		fv.Set(rv)
		return nil
	}
	return fmt.Errorf("cannot store %T in a %s field", v, fv.Type())
}


// ---------------------------------------------------------------------------
// Column option getters / setters
//
// Every Column option can be read and changed at run time, by column key:
//
//	g.SetColumnReadOnly("Name", true)
//	ro := g.ColumnReadOnly("Name")
//
// Setters return false when the key is unknown. Getters return the zero value
// for an unknown key (use ColumnByKey if you need to tell the difference).
// Every setter repaints the grid, and closes the editor if the change makes
// the cell being edited invalid (e.g. the column became read-only).
//
// Key has a getter only: for grids built with New the key is what links a
// column to its struct field, so changing it would break the data lookup.
// ---------------------------------------------------------------------------

// String makes ColType print as text ("Text", "Number", ...).
func (t ColType) String() string {
	switch t {
	case ColText:
		return "Text"
	case ColNumber:
		return "Number"
	case ColDate:
		return "Date"
	case ColInt:
		return "Int"
	}
	return "ColType(" + strconv.Itoa(int(t)) + ")"
}

// ColumnIndex returns the index of the column with this key, or -1.
func (g *DataGrid) ColumnIndex(key string) int { return g.colIndex(key) }

// ColumnByKey returns a copy of the column definition.
func (g *DataGrid) ColumnByKey(key string) (Column, bool) {
	i := g.colIndex(key)
	if i < 0 {
		return Column{}, false
	}
	return g.cols[i], true
}

// UpdateColumn is the generic setter: fn receives a pointer to a copy of the
// column, whatever it changes is applied and the grid is repainted. Key
// changes are ignored. Use it to change several options in one repaint.
func (g *DataGrid) UpdateColumn(key string, fn func(c *Column)) bool {
	i := g.colIndex(key)
	if i < 0 || fn == nil {
		return false
	}
	old := g.cols[i]
	c := old
	fn(&c)
	c.Key = old.Key
	g.cols[i] = c

	if g.edit != nil && g.edit.col == i && (c.ReadOnly || c.Type != old.Type) {
		g.edit = nil // the open editor no longer fits this column
	}
	if c.Type != old.Type && g.sortCol == i {
		g.rebuildView() // sort order depends on the column type
	}
	g.colsGen++ // renderer rebuilds header alignment / pools
	g.redraw()
	return true
}

// --- Key (read only) ---

func (g *DataGrid) ColumnKey(col int) string {
	if col < 0 || col >= len(g.cols) {
		return ""
	}
	return g.cols[col].Key
}

// --- Title ---

func (g *DataGrid) ColumnTitle(key string) string {
	c, _ := g.ColumnByKey(key)
	return c.Title
}
func (g *DataGrid) SetColumnTitle(key, title string) bool {
	return g.UpdateColumn(key, func(c *Column) { c.Title = title })
}

// --- Type ---

func (g *DataGrid) ColumnType(key string) ColType {
	c, _ := g.ColumnByKey(key)
	return c.Type
}
func (g *DataGrid) SetColumnType(key string, t ColType) bool {
	return g.UpdateColumn(key, func(c *Column) { c.Type = t })
}

// --- WidthPct ---

func (g *DataGrid) ColumnWidthPct(key string) float32 {
	c, _ := g.ColumnByKey(key)
	return c.WidthPct
}

// SetColumnWidthPct sets the width as a percent of the grid. It also drops a
// width the user dragged this column to, so the new value takes effect.
func (g *DataGrid) SetColumnWidthPct(key string, pct float32) bool {
	if pct < 0 {
		pct = 0
	}
	if i := g.colIndex(key); i >= 0 && i < len(g.userW) {
		g.userW[i] = 0
	}
	return g.UpdateColumn(key, func(c *Column) { c.WidthPct = pct })
}

// --- MinWidth ---

func (g *DataGrid) ColumnMinWidth(key string) float32 {
	c, _ := g.ColumnByKey(key)
	return c.MinWidth
}
func (g *DataGrid) SetColumnMinWidth(key string, px float32) bool {
	if px < 0 {
		px = 0
	}
	return g.UpdateColumn(key, func(c *Column) { c.MinWidth = px })
}

// --- Align ---

func (g *DataGrid) ColumnAlign(key string) fyne.TextAlign {
	c, _ := g.ColumnByKey(key)
	return c.Align
}
func (g *DataGrid) SetColumnAlign(key string, a fyne.TextAlign) bool {
	return g.UpdateColumn(key, func(c *Column) { c.Align = a })
}

// --- Decimals (number columns) ---

func (g *DataGrid) ColumnDecimals(key string) int {
	c, _ := g.ColumnByKey(key)
	return c.Decimals
}
func (g *DataGrid) SetColumnDecimals(key string, n int) bool {
	if n < 0 {
		n = 0
	}
	return g.UpdateColumn(key, func(c *Column) { c.Decimals = n })
}

// --- Thousands (number and int columns) ---

func (g *DataGrid) ColumnThousands(key string) bool {
	c, _ := g.ColumnByKey(key)
	return c.Thousands
}
func (g *DataGrid) SetColumnThousands(key string, on bool) bool {
	return g.UpdateColumn(key, func(c *Column) { c.Thousands = on })
}

// --- Formatter ---

func (g *DataGrid) ColumnFormatter(key string) func(v any) string {
	c, _ := g.ColumnByKey(key)
	return c.Formatter
}

// SetColumnFormatter overrides how values are displayed; nil removes it.
func (g *DataGrid) SetColumnFormatter(key string, f func(v any) string) bool {
	return g.UpdateColumn(key, func(c *Column) { c.Formatter = f })
}

// --- DateLayout (date columns) ---

func (g *DataGrid) ColumnDateLayout(key string) string {
	c, _ := g.ColumnByKey(key)
	return c.DateLayout
}
func (g *DataGrid) SetColumnDateLayout(key, layout string) bool {
	return g.UpdateColumn(key, func(c *Column) { c.DateLayout = layout })
}

// --- HideZero (number columns) ---

func (g *DataGrid) ColumnHideZero(key string) bool {
	c, _ := g.ColumnByKey(key)
	return c.HideZero
}
func (g *DataGrid) SetColumnHideZero(key string, on bool) bool {
	return g.UpdateColumn(key, func(c *Column) { c.HideZero = on })
}

// --- ReadOnly ---

// ColumnReadOnly reports the column's own flag. The cells are only really
// editable when the grid itself is not read-only either: see
// IsColumnEditable.
func (g *DataGrid) ColumnReadOnly(key string) bool {
	c, _ := g.ColumnByKey(key)
	return c.ReadOnly
}
func (g *DataGrid) SetColumnReadOnly(key string, on bool) bool {
	return g.UpdateColumn(key, func(c *Column) { c.ReadOnly = on })
}

// SetAllColumnsReadOnly sets the ReadOnly flag of every column at once.
func (g *DataGrid) SetAllColumnsReadOnly(on bool) {
	for i := range g.cols {
		g.cols[i].ReadOnly = on
	}
	if on {
		g.edit = nil
	}
	g.colsGen++
	g.redraw()
}

// IsColumnEditable is the effective answer: the grid is not read-only, the
// data source can store values and the column is not read-only.
func (g *DataGrid) IsColumnEditable(key string) bool {
	i := g.colIndex(key)
	return i >= 0 && g.isEditable(i)
}

// ---------------------------------------------------------------------------
// Grid option getters / setters (the exported bool fields, with repaint)
// ---------------------------------------------------------------------------

func (g *DataGrid) IsReadOnly() bool { return g.ReadOnly }
func (g *DataGrid) SetReadOnly(on bool) {
	g.ReadOnly = on
	if on {
		g.edit = nil
	}
	g.redraw()
}

func (g *DataGrid) IsEnterMovesNext() bool    { return g.EnterMovesNext }
func (g *DataGrid) SetEnterMovesNext(on bool) { g.EnterMovesNext = on }

// EnterFirstColumn / EnterLastColumn return the column keys Enter walks
// between ("" = the grid's first / last column).
func (g *DataGrid) EnterFirstColumn() string { return g.EnterFirstCol }
func (g *DataGrid) EnterLastColumn() string  { return g.EnterLastCol }

// SetEnterFirstColumn sets the column the cursor goes to on the next / new
// row. "" restores the grid's first column. Returns false for an unknown key.
func (g *DataGrid) SetEnterFirstColumn(key string) bool {
	if key != "" && g.colIndex(key) < 0 {
		return false
	}
	g.EnterFirstCol = key
	return true
}

// SetEnterLastColumn sets the column after which Enter wraps to the next row
// (and adds a row on the last one). "" restores the grid's last column.
// Returns false for an unknown key.
func (g *DataGrid) SetEnterLastColumn(key string) bool {
	if key != "" && g.colIndex(key) < 0 {
		return false
	}
	g.EnterLastCol = key
	return true
}

func (g *DataGrid) IsAutoAddRow() bool    { return g.AutoAddRow }
func (g *DataGrid) SetAutoAddRow(on bool) { g.AutoAddRow = on }

func (g *DataGrid) IsSortable() bool { return g.Sortable }

// SetSortable turns header-click sorting on or off. Turning it off also
// clears the current sort and returns the rows to their original order.
func (g *DataGrid) SetSortable(on bool) {
	g.Sortable = on
	if !on && g.sortCol >= 0 {
		g.sortCol, g.sortAsc = -1, true
		g.rebuildView()
	}
	g.redraw()
}

func (g *DataGrid) IsStriped() bool { return g.Striped }
func (g *DataGrid) SetStriped(on bool) {
	g.Striped = on
	g.redraw()
}

func (g *DataGrid) IsResizable() bool { return g.Resizable }

// SetResizable allows or forbids dragging header edges. Turning it off also
// undoes widths the user already dragged.
func (g *DataGrid) SetResizable(on bool) {
	g.Resizable = on
	if !on {
		g.ResetColumnWidths()
		return
	}
	g.redraw()
}


// ---------------------------------------------------------------------------
// Cell font / color: set DataGrid.StyleCell, then call Repaint() (or Reload())
// whenever your rules or data changed.
// ---------------------------------------------------------------------------

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// styleOf asks StyleCell for the style of one cell.
func (g *DataGrid) styleOf(ri, ci int) CellStyle {
	if g.StyleCell == nil {
		return CellStyle{}
	}
	st, _ := g.StyleCell(ri, g.cols[ci].Key)
	return st
}

// ---------------------------------------------------------------------------
// Enter key hook (see DataGrid.OnEnter)
// ---------------------------------------------------------------------------

// fireEnter calls OnEnter for the cursor row and reports whether it handled
// the key. With no cursor row it does nothing.
func (g *DataGrid) fireEnter() bool {
	if g.OnEnter == nil || g.selected < 0 || g.selected >= g.src.Len() {
		return false
	}
	return g.OnEnter(g.selected, g.ColumnKey(g.curCol), g.rowOf(g.selected))
}

// ShiftDown reports whether Shift is held (handy inside OnEnter).
func (g *DataGrid) ShiftDown() bool { return g.shift }

// ---------------------------------------------------------------------------
// Multi-row selection
//
// The grid always has a cursor (one cell: DataGrid.selected / curCol). On top
// of that the user can select several rows:
//
//     Ctrl/Cmd+click            toggle one row
//     Shift+click               select the range anchor..clicked row
//     Ctrl/Cmd+Shift+click      add that range to the current selection
//     Shift+Up/Down/PgUp/PgDn   grow or shrink the range from the keyboard
//     Ctrl/Cmd+A                select every row
//     Esc                       back to the cursor row alone
//     right-click inside it     keeps it; the menu deletes all selected rows
//
// Rows are remembered by SOURCE index, so sorting does not disturb them.
// Inserting / deleting shifts source indices, so those reset the selection.
// While selOn is false the selection is simply the cursor row (the grid's
// classic behaviour). Selected rows live in a bitset: a million rows cost
// about 125 KB however many are selected.
// ---------------------------------------------------------------------------

// rowSet is a bitset of source row indices.
type rowSet struct {
	w []uint64
	n int // number of rows in the set
}

func (s *rowSet) has(i int) bool {
	return i >= 0 && i>>6 < len(s.w) && s.w[i>>6]&(uint64(1)<<(uint(i)&63)) != 0
}

func (s *rowSet) add(i int) {
	if i < 0 {
		return
	}
	if need := i>>6 + 1; need > len(s.w) {
		s.w = append(s.w, make([]uint64, need-len(s.w))...)
	}
	if bit := uint64(1) << (uint(i) & 63); s.w[i>>6]&bit == 0 {
		s.w[i>>6] |= bit
		s.n++
	}
}

func (s *rowSet) del(i int) {
	if s.has(i) {
		s.w[i>>6] &^= uint64(1) << (uint(i) & 63)
		s.n--
	}
}

func (s *rowSet) clear() {
	for i := range s.w {
		s.w[i] = 0
	}
	s.n = 0
}

// fill makes the set 0..n-1.
func (s *rowSet) fill(n int) {
	s.w = make([]uint64, (n+63)>>6)
	for i := range s.w {
		s.w[i] = ^uint64(0)
	}
	if r := uint(n) & 63; r != 0 {
		s.w[len(s.w)-1] = uint64(1)<<r - 1
	}
	s.n = n
}

// trim drops every index >= n.
func (s *rowSet) trim(n int) {
	keep := (n + 63) >> 6
	if keep < len(s.w) {
		s.w = s.w[:keep]
	}
	if r := uint(n) & 63; r != 0 && keep > 0 && len(s.w) == keep {
		s.w[keep-1] &= uint64(1)<<r - 1
	}
	s.n = 0
	for _, word := range s.w {
		s.n += bits.OnesCount64(word)
	}
}

// each calls f for every index, ascending.
func (s *rowSet) each(f func(i int)) {
	for k, word := range s.w {
		for word != 0 {
			f(k<<6 + bits.TrailingZeros64(word))
			word &= word - 1
		}
	}
}

// rowSelected reports whether source row ri is drawn as selected.
func (g *DataGrid) rowSelected(ri int) bool {
	if g.cellOn { // a cell rectangle is drawn by the renderer, not as a full-row highlight
		return false
	}
	if g.selOn {
		return g.sel.has(ri)
	}
	return ri == g.selected
}

// collapseTo makes ri the only selected row and the anchor for Shift ranges.
func (g *DataGrid) collapseTo(ri int) {
	g.clearCellState()
	g.selOn = false
	g.sel.clear()
	g.anchor = ri
}

// dropSelection forgets the selection and the anchor (source indices shifted,
// or the cursor went away).
func (g *DataGrid) dropSelection() {
	g.clearCellState()
	g.anchorCol = -1
	g.selOn = false
	g.sel.clear()
	g.anchor = -1
}

// trimSelection removes selected rows that no longer exist (n = new row count).
func (g *DataGrid) trimSelection(n int) {
	if g.selOn {
		g.sel.trim(n)
	}
	if g.anchor >= n {
		g.anchor = -1
	}
	if g.cellOn && (g.anchor < 0 || g.selected < 0 || g.selected >= n) {
		g.cellOn = false // the rectangle's anchor or cursor row is gone
	}
}

// toggleRow is Ctrl/Cmd+click: flip one row, keep the rest.
func (g *DataGrid) toggleRow(ri int) {
	g.clearCellState()
	if !g.selOn { // start from what is selected now: the cursor row
		g.sel.clear()
		if g.selected >= 0 && g.selected < g.src.Len() {
			g.sel.add(g.selected)
		}
		g.selOn = true
	}
	if g.sel.has(ri) {
		g.sel.del(ri)
	} else {
		g.sel.add(ri)
	}
	g.anchor = ri
}

// rangeSelect selects the display-order range anchor..ri. With keep the range
// is added to the current selection, otherwise it replaces it. The anchor does
// not move, so the range can be grown and shrunk around it.
func (g *DataGrid) rangeSelect(ri int, keep bool) {
	g.clearCellState()
	a := g.anchor
	if a < 0 || a >= len(g.pos) {
		a = g.selected
	}
	if a < 0 || a >= len(g.pos) {
		a = ri
	}
	g.anchor = a
	switch {
	case !keep:
		g.sel.clear()
	case !g.selOn:
		g.sel.clear()
		if g.selected >= 0 && g.selected < len(g.pos) {
			g.sel.add(g.selected)
		}
	}
	g.selOn = true
	v1, v2 := g.pos[a], g.pos[ri]
	if v1 > v2 {
		v1, v2 = v2, v1
	}
	for v := v1; v <= v2; v++ {
		g.sel.add(g.view[v])
	}
}

// collapseToCursor (Esc) drops a multi-row selection or cell rectangle, keeping the cursor cell.
func (g *DataGrid) collapseToCursor() {
	if !g.selOn && !g.cellOn {
		return
	}
	g.collapseTo(g.selected)
	g.anchorCol = g.curCol
	g.redraw()
	g.fireSelection()
}

// moveCursorKeepSelection moves the cursor without touching the selection
// (right-click inside a multi-row selection).
func (g *DataGrid) moveCursorKeepSelection(vr, col int) {
	if vr < 0 || vr >= len(g.view) {
		return
	}
	if g.edit != nil && !g.commitEdit() {
		g.endEdit(false)
	}
	prev := g.selected
	g.selected = g.view[vr]
	if col >= 0 && col < len(g.cols) {
		g.curCol = col
	}
	g.focusGrid()
	g.redraw()
	if g.OnSelected != nil && prev != g.selected {
		g.OnSelected(g.selected, g.rowOf(g.selected))
	}
}

func (g *DataGrid) fireSelection() {
	if g.OnSelectionChanged != nil {
		g.OnSelectionChanged(g.SelectedIndices())
	}
}

// MouseDown records the modifier keys (Ctrl / Cmd / Shift) held during a
// click. Fyne's Tapped event does not carry them. MouseUp is needed to satisfy
// desktop.Mouseable; Fyne calls MouseDown, MouseUp and only then Tapped.
func (g *DataGrid) MouseDown(ev *desktop.MouseEvent) { g.mods = ev.Modifier }
func (g *DataGrid) MouseUp(*desktop.MouseEvent)      {}

// TypedShortcut handles Ctrl+A (Cmd+A on macOS): select every row, and Ctrl+C
// (Cmd+C): copy the cell rectangle, the selected rows (tab-separated) or, with
// neither, the cursor cell. Ctrl+V (Cmd+V) pastes, see PasteCells.
func (g *DataGrid) TypedShortcut(s fyne.Shortcut) {
	if g.edit != nil {
		return
	}
	if fn := g.shortcuts[s.ShortcutName()]; fn != nil {
		idx := -1
		var rw Row
		if g.selected >= 0 && g.selected < g.src.Len() {
			idx, rw = g.selected, g.rowOf(g.selected)
		}
		fn(idx, g.ColumnKey(g.curCol), rw)
		return
	}
	switch sc := s.(type) {
	case *desktop.CustomShortcut:
		// Alt+Left / Alt+Right resize the cursor column (Shift = bigger step).
		if sc.Modifier&fyne.KeyModifierAlt == 0 || (sc.KeyName != fyne.KeyLeft && sc.KeyName != fyne.KeyRight) {
			return
		}
		step := keyResizeStep
		if sc.Modifier&fyne.KeyModifierShift != 0 {
			step = keyResizeStepShift
		}
		if sc.KeyName == fyne.KeyLeft {
			step = -step
		}
		g.resizeCursorCol(step)
	case *fyne.ShortcutSelectAll:
		g.SelectAll()
	case *fyne.ShortcutPaste:
		_ = g.PasteCells()
	case *fyne.ShortcutCopy:
		switch {
		case g.cellOn:
			_ = g.CopySelectedCells()
		case g.selOn && g.sel.n > 1:
			_ = g.copyRows('\t')
		default:
			_ = g.CopyCursorCell()
		}
	}
}

// ---- public API ----

func (g *DataGrid) IsMultiSelect() bool { return g.MultiSelect }

// SetMultiSelect turns multi-row selection on or off. Turning it off also
// shrinks the selection to the cursor row.
func (g *DataGrid) SetMultiSelect(on bool) {
	g.MultiSelect = on
	if !on && g.selOn {
		g.collapseTo(g.selected)
		g.redraw()
		g.fireSelection()
	}
}

// SelectedCount is the number of selected rows.
func (g *DataGrid) SelectedCount() int {
	if g.selOn {
		return g.sel.n
	}
	if g.selected >= 0 && g.selected < g.src.Len() {
		return 1
	}
	return 0
}

// IsRowSelected reports whether the row at source index idx is selected.
func (g *DataGrid) IsRowSelected(idx int) bool {
	return idx >= 0 && idx < g.src.Len() && g.rowSelected(idx)
}

// SelectedIndices returns the selected rows as indices in your data, ascending
// (nothing selected = empty). With a single selected row it is just the cursor
// row, the same as SelectedIndex.
func (g *DataGrid) SelectedIndices() []int {
	n := g.src.Len()
	if !g.selOn {
		if g.selected >= 0 && g.selected < n {
			return []int{g.selected}
		}
		return nil
	}
	out := make([]int, 0, g.sel.n)
	g.sel.each(func(i int) {
		if i < n {
			out = append(out, i)
		}
	})
	return out
}

// SelectedRows returns a snapshot (see Row) of every selected row, in data order.
func (g *DataGrid) SelectedRows() []Row {
	idxs := g.SelectedIndices()
	out := make([]Row, len(idxs))
	for k, i := range idxs {
		out[k] = g.rowOf(i)
	}
	return out
}

// SetSelectedIndices selects exactly these rows (indices in your data; invalid
// ones are ignored). The cursor goes to the last valid one and the grid
// scrolls to it. An empty list clears the selection but leaves the cursor
// where it was. Without MultiSelect only the last index is used.
func (g *DataGrid) SetSelectedIndices(idxs []int) {
	g.ensureView()
	if !g.MultiSelect {
		if len(idxs) == 0 {
			g.SelectIndex(-1)
		} else {
			g.SelectIndex(idxs[len(idxs)-1])
		}
		return
	}
	g.settleEditor()
	g.clearCellState()
	n := g.src.Len()
	prev := g.selected
	g.sel.clear()
	last := -1
	for _, i := range idxs {
		if i >= 0 && i < n {
			g.sel.add(i)
			last = i
		}
	}
	g.selOn = true
	g.anchor = last
	if last >= 0 {
		g.selected = last
		g.ensureVisible(g.pos[last])
	}
	g.redraw()
	if g.OnSelected != nil && g.selected >= 0 && prev != g.selected {
		g.OnSelected(g.selected, g.rowOf(g.selected))
	}
	g.fireSelection()
}

// SelectAll selects every row (Ctrl/Cmd+A). The cursor stays where it is.
func (g *DataGrid) SelectAll() {
	if !g.MultiSelect {
		return
	}
	g.ensureView()
	n := g.src.Len()
	if n == 0 {
		return
	}
	g.settleEditor()
	g.clearCellState()
	g.sel.fill(n)
	g.selOn = true
	if g.anchor < 0 || g.anchor >= n {
		g.anchor = g.selected
	}
	g.redraw()
	g.fireSelection()
}

// ClearSelection deselects every row. The cursor cell stays where it is.
func (g *DataGrid) ClearSelection() {
	if !g.MultiSelect {
		g.SelectIndex(-1)
		return
	}
	g.clearCellState()
	g.sel.clear()
	g.selOn = true
	g.redraw()
	g.fireSelection()
}

// DeleteSelectedRows removes every selected row. BeforeRowDelete is asked
// about each one first and any error cancels the whole delete. Rows are
// deleted from the bottom up so the source indices stay valid; OnRowDeleted
// fires once per row, highest index first. The cursor stays at the same place
// on screen, with nothing else selected.
func (g *DataGrid) DeleteSelectedRows() error {
	if !g.canDelete() {
		return errRowOpUnavailable
	}
	g.settleEditor()
	idxs := g.SelectedIndices()
	switch len(idxs) {
	case 0:
		return nil
	case 1:
		return g.DeleteRowAt(idxs[0])
	}
	if g.BeforeRowDelete != nil {
		for _, i := range idxs {
			if err := g.BeforeRowDelete(i, g.rowOf(i)); err != nil {
				return err
			}
		}
	}
	vr := g.cursorViewRow()
	if vr < 0 {
		vr = g.pos[idxs[0]]
	}
	var done []int
	var failed error
	for k := len(idxs) - 1; k >= 0; k-- {
		if failed = g.DeleteRow(idxs[k]); failed != nil {
			break
		}
		done = append(done, idxs[k])
	}
	if len(done) == 0 {
		return failed
	}
	g.structureChanged()
	if n := g.src.Len(); n == 0 {
		g.selected = -1
		g.redraw()
	} else {
		g.selected = g.view[clampInt(vr, 0, n-1)]
		g.anchor = g.selected
		g.ensureVisible(g.pos[g.selected])
		g.redraw()
		if g.OnSelected != nil {
			g.OnSelected(g.selected, g.rowOf(g.selected))
		}
	}
	g.fireSelection()
	if g.OnRowDeleted != nil {
		for _, i := range done {
			g.OnRowDeleted(i)
		}
	}
	return failed
}

// ---------------------------------------------------------------------------
// Multi-cell selection
//
// With CellSelect on (the default) the user can select a rectangle of cells:
//
//     Shift+click               rectangle anchor cell..clicked cell
//     Shift+Arrows/PgUp/PgDn    grow or shrink it from the keyboard
//     Shift+Home/End            ... to the first / last column
//     mouse drag                rectangle from the pressed cell to the one under
//                               the pointer (scrolls when you leave the grid)
//     Ctrl/Cmd+C                copy it as tab-separated text (spreadsheet-ready)
//     Delete / Backspace        clear its editable cells
//     Esc / any plain click     back to the cursor cell alone
//     right-click inside it     keeps it; the menu offers "Copy N cells"
//
// The rectangle runs from the anchor cell (row g.anchor, column g.anchorCol,
// both set by the last plain click or cursor move) to the cursor cell
// (g.selected, g.curCol), so it is stored as two cells and never as a list.
// Rows are remembered by SOURCE index, but the rectangle is taken in display
// order, so it is dropped when the grid is sorted. Row selection (Ctrl+click,
// Ctrl+A, rowSet) is a separate thing: starting one drops the rectangle.
// While cellOn is false there is no rectangle, just the cursor cell.
// ---------------------------------------------------------------------------

// CellRange is a rectangle of selected cells. Rows are indices in YOUR data,
// in display order (top to bottom); Cols are column indices, ascending. The
// selected cells are every (Rows[i], Cols[j]) pair.
type CellRange struct {
	Rows []int
	Cols []int
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// cellRect returns the rectangle as display rows v1..v2 and columns c1..c2.
// ok is false when no (valid) rectangle is selected.
func (g *DataGrid) cellRect() (v1, v2, c1, c2 int, ok bool) {
	if !g.cellOn || g.anchor < 0 || g.anchor >= len(g.pos) || g.selected < 0 || g.selected >= len(g.pos) ||
		g.anchorCol < 0 || g.anchorCol >= len(g.cols) || g.curCol < 0 || g.curCol >= len(g.cols) {
		return 0, 0, 0, 0, false
	}
	v1, v2 = g.pos[g.anchor], g.pos[g.selected]
	if v1 > v2 {
		v1, v2 = v2, v1
	}
	c1, c2 = g.anchorCol, g.curCol
	if c1 > c2 {
		c1, c2 = c2, c1
	}
	return v1, v2, c1, c2, true
}

// clearCellState forgets the rectangle (not the anchor) and reports it.
func (g *DataGrid) clearCellState() {
	if g.cellOn {
		g.cellOn = false
		g.fireCellSelection()
	}
}

// collapseCells shrinks a rectangle to the cursor cell.
func (g *DataGrid) collapseCells() {
	if g.cellOn {
		g.collapseTo(g.selected)
		g.anchorCol = g.curCol
	}
}

func (g *DataGrid) fireCellSelection() {
	if g.OnCellSelectionChanged != nil {
		g.OnCellSelectionChanged(g.SelectedCellRange())
	}
}

// extendCell makes the rectangle run from its anchor cell to (ri, col), where
// ri is a source row index. The cursor moves there too. With no anchor yet the
// current cursor cell becomes the anchor. Row selection is dropped.
func (g *DataGrid) extendCell(ri, col int) {
	if g.anchor < 0 || g.anchor >= len(g.pos) {
		g.anchor, g.anchorCol = g.selected, g.curCol
	}
	if g.anchor < 0 || g.anchor >= len(g.pos) { // no cursor either
		g.anchor, g.anchorCol = ri, col
	}
	if g.anchorCol < 0 || g.anchorCol >= len(g.cols) {
		g.anchorCol = col
	}
	if g.selOn { // the bitset is only worth clearing (O(rows/64)) if rows were selected
		g.selOn = false
		g.sel.clear()
	}
	was := g.cellOn
	g.cellOn = g.anchor != ri || g.anchorCol != col
	g.selected, g.curCol = ri, col
	if was || g.cellOn {
		g.fireCellSelection()
	}
}

// startsCellDrag reports whether a mouse drag that began at (x, y) selects
// cells: it must start on a body cell, without Ctrl/Cmd (that toggles rows).
func (g *DataGrid) startsCellDrag(x, y float32) bool {
	if !g.CellSelect || g.rowH <= 0 || y < g.headerH || g.mods&(fyne.KeyModifierControl|fyne.KeyModifierSuper) != 0 {
		return false
	}
	return g.topRow+int((y-g.headerH)/g.rowH) < len(g.view) && g.colAt(x) >= 0
}

// dragSelect extends the rectangle to the cell under the pointer while a drag
// that started on a cell is running. Past the edge of the grid it scrolls one
// row / column per drag event.
func (g *DataGrid) dragSelect(p fyne.Position) {
	if !g.dragCells || !g.CellSelect || len(g.view) == 0 || len(g.cols) == 0 || g.rowH <= 0 {
		return
	}
	bodyBottom := g.Size().Height
	if g.hasH {
		bodyBottom -= g.sbH
	}
	var vr int
	switch {
	case p.Y < g.headerH:
		vr = g.topRow - 1
	case p.Y >= bodyBottom:
		vr = g.topRow + maxInt(g.visRows, 1)
	default:
		vr = g.topRow + int((p.Y-g.headerH)/g.rowH)
	}
	vr = clampInt(vr, 0, len(g.view)-1)
	col := g.colAt(p.X)
	if col < 0 {
		switch {
		case p.X < 0:
			col = g.curCol - 1
		case p.X >= g.viewW:
			col = g.curCol + 1
		default:
			col = g.curCol
		}
	}
	col = clampInt(col, 0, len(g.cols)-1)
	ri := g.view[vr]
	if ri == g.selected && col == g.curCol {
		return
	}
	prev, wasMulti := g.selected, g.selOn
	g.extendCell(ri, col)
	g.ensureVisible(vr)
	g.ensureColVisible(col)
	g.redraw()
	if g.OnSelected != nil && prev != g.selected {
		g.OnSelected(g.selected, g.rowOf(g.selected))
	}
	if prev != g.selected || wasMulti {
		g.fireSelection()
	}
}

// clearCellRange empties every editable cell of the rectangle. Each cell goes
// through BeforeCellChange, an error leaves that cell alone, and
// OnCellChanged fires once per cell that really changed.
func (g *DataGrid) clearCellRange() {
	r := g.SelectedCellRange()
	var evs []CellChange
	for _, ri := range r.Rows {
		for _, ci := range r.Cols {
			if !g.isEditable(ci) {
				continue
			}
			col := g.cols[ci]
			val, _ := col.parse("")
			ev, changed, err := g.applyChange(ri, ci, g.src.Value(ri, col.Key), val)
			if err == nil && changed {
				evs = append(evs, ev)
			}
		}
	}
	g.redraw()
	if g.OnCellChanged != nil {
		for _, ev := range evs {
			g.OnCellChanged(ev)
		}
	}
}

// ---- public API ----

func (g *DataGrid) IsCellSelect() bool { return g.CellSelect }

// SetCellSelect turns cell-rectangle selection on or off. Turning it off also
// shrinks a rectangle to the cursor cell.
func (g *DataGrid) SetCellSelect(on bool) {
	g.CellSelect = on
	if !on {
		g.ClearCellSelection()
	}
}

// SelectedCellRange returns the selected cells. With no rectangle it is the
// cursor cell alone; with no cursor either, both slices are empty.
func (g *DataGrid) SelectedCellRange() CellRange {
	g.ensureView()
	if v1, v2, c1, c2, ok := g.cellRect(); ok {
		rows := make([]int, 0, v2-v1+1)
		for v := v1; v <= v2 && v < len(g.view); v++ {
			rows = append(rows, g.view[v])
		}
		cols := make([]int, 0, c2-c1+1)
		for c := c1; c <= c2; c++ {
			cols = append(cols, c)
		}
		return CellRange{Rows: rows, Cols: cols}
	}
	if g.selected >= 0 && g.selected < g.src.Len() && g.curCol >= 0 && g.curCol < len(g.cols) {
		return CellRange{Rows: []int{g.selected}, Cols: []int{g.curCol}}
	}
	return CellRange{}
}

// IsCellSelected reports whether the cell (row = index in your data, col =
// column index) is in the rectangle, or is the cursor cell when there is none.
func (g *DataGrid) IsCellSelected(row, col int) bool {
	if row < 0 || row >= len(g.pos) || col < 0 || col >= len(g.cols) {
		return false
	}
	if v1, v2, c1, c2, ok := g.cellRect(); ok {
		v := g.pos[row]
		return v >= v1 && v <= v2 && col >= c1 && col <= c2
	}
	return row == g.selected && col == g.curCol
}

// SelectCells selects the rectangle between two cells (rows are indices in
// your data, columns are column indices) and moves the cursor to the second
// one. Invalid cells are ignored.
func (g *DataGrid) SelectCells(fromRow, fromCol, toRow, toCol int) {
	g.ensureView()
	n := g.src.Len()
	if fromRow < 0 || fromRow >= n || toRow < 0 || toRow >= n ||
		fromCol < 0 || fromCol >= len(g.cols) || toCol < 0 || toCol >= len(g.cols) {
		return
	}
	g.settleEditor()
	prev, wasMulti := g.selected, g.selOn
	g.anchor, g.anchorCol = fromRow, fromCol
	g.extendCell(toRow, toCol)
	g.ensureVisible(g.pos[toRow])
	g.ensureColVisible(toCol)
	g.redraw()
	if g.OnSelected != nil && prev != g.selected {
		g.OnSelected(g.selected, g.rowOf(g.selected))
	}
	if prev != g.selected || wasMulti {
		g.fireSelection()
	}
}

// ClearCellSelection shrinks a cell rectangle to the cursor cell.
func (g *DataGrid) ClearCellSelection() {
	if g.cellOn {
		g.collapseToCursor()
	}
}

// tsvField quotes a field the way spreadsheets expect when it holds a tab,
// a line break or a double quote.
func tsvField(s string) string {
	if strings.ContainsAny(s, "\t\r\n\"") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

// CopySelectedCells puts the selected cells on the clipboard as tab-separated
// text (rows joined by line breaks, no header), ready to paste into a
// spreadsheet. With no rectangle it copies the cursor cell.
func (g *DataGrid) CopySelectedCells() error {
	r := g.SelectedCellRange()
	if len(r.Rows) == 0 || len(r.Cols) == 0 {
		return fmt.Errorf("no cell selected")
	}
	lines := make([]string, len(r.Rows))
	fields := make([]string, len(r.Cols))
	for i, ri := range r.Rows {
		for j, ci := range r.Cols {
			fields[j] = tsvField(g.exportText(ri, g.cols[ci]))
		}
		lines[i] = strings.Join(fields, "\t")
	}
	return g.setClipboard(strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// Copy / export (right-click menu: Copy cell, Copy row(s) as CSV / PSV, Export CSV)
//
// The text is what the grid shows (FormatCell, Formatter, Decimals, DateLayout,
// HideZero), except that number columns leave out thousand separators so the
// values paste and import as numbers. Copies hold data rows only; an exported
// file starts with a header row of the column titles. Rows come out in display
// order (the current sort). CSV separates with ',' and PSV with '|'. Both go
// through encoding/csv, so a field holding the separator, a quote or a line
// break is quoted. Copy and Export need DataGrid.Window.
// ---------------------------------------------------------------------------

var errNoWindow = fmt.Errorf("copy / export needs the grid's window: set DataGrid.Window")

// exportText is the text of one cell for copying / exporting.
func (g *DataGrid) exportText(ri int, col Column) string {
	if g.FormatCell != nil {
		if s, ok := g.FormatCell(ri, col.Key); ok {
			return s
		}
	}
	col.Thousands = false // col is a copy; a Formatter still wins over this
	return col.format(g.src.Value(ri, col.Key))
}

// exportRows returns source indices in display order: every row, or only the
// selected ones.
func (g *DataGrid) exportRows(selectedOnly bool) []int {
	g.ensureView()
	if !selectedOnly {
		return append([]int(nil), g.view...)
	}
	if !g.selOn {
		if g.selected >= 0 && g.selected < len(g.pos) {
			return []int{g.selected}
		}
		return nil
	}
	out := make([]int, 0, g.sel.n)
	for _, ri := range g.view {
		if g.sel.has(ri) {
			out = append(out, ri)
		}
	}
	return out
}

// writeDelimited writes rows (source indices) as delimited text.
func (g *DataGrid) writeDelimited(w io.Writer, comma rune, rows []int, header bool) error {
	cw := csv.NewWriter(w)
	cw.Comma = comma
	rec := make([]string, len(g.cols))
	if header {
		for i, c := range g.cols {
			rec[i] = c.Title
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	for _, ri := range rows {
		for i, c := range g.cols {
			rec[i] = g.exportText(ri, c)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteCSV writes a header row and then the rows as CSV, in display order.
// selectedOnly limits it to the selected rows.
func (g *DataGrid) WriteCSV(w io.Writer, selectedOnly bool) error {
	return g.writeDelimited(w, ',', g.exportRows(selectedOnly), true)
}

// WritePSV is WriteCSV with '|' as the separator.
func (g *DataGrid) WritePSV(w io.Writer, selectedOnly bool) error {
	return g.writeDelimited(w, '|', g.exportRows(selectedOnly), true)
}

func (g *DataGrid) setClipboard(s string) error {
	if g.Window == nil {
		return errNoWindow
	}
	g.Window.Clipboard().SetContent(s)
	return nil
}

// CopyCursorCell puts the text of the cursor cell on the clipboard.
func (g *DataGrid) CopyCursorCell() error {
	if g.selected < 0 || g.selected >= g.src.Len() || g.curCol < 0 || g.curCol >= len(g.cols) {
		return fmt.Errorf("no cell selected")
	}
	return g.setClipboard(g.exportText(g.selected, g.cols[g.curCol]))
}

func (g *DataGrid) copyRows(comma rune) error {
	rows := g.exportRows(true)
	if len(rows) == 0 {
		return fmt.Errorf("no row selected")
	}
	var b strings.Builder
	if err := g.writeDelimited(&b, comma, rows, false); err != nil {
		return err
	}
	return g.setClipboard(b.String())
}

// CopySelectedCSV puts the selected rows on the clipboard as CSV (no header).
func (g *DataGrid) CopySelectedCSV() error { return g.copyRows(',') }

// CopySelectedPSV puts the selected rows on the clipboard as PSV (no header).
func (g *DataGrid) CopySelectedPSV() error { return g.copyRows('|') }

// ExportCSV opens a save dialog and writes the grid there as CSV with a header
// row: every row, or only the selected ones. The file is UTF-8 with a byte
// order mark so Excel reads accented text correctly. The data is captured when
// the dialog opens. Errors that happen later are shown in a dialog.
func (g *DataGrid) ExportCSV(selectedOnly bool) error {
	if g.Window == nil {
		return errNoWindow
	}
	rows := g.exportRows(selectedOnly)
	if len(rows) == 0 {
		return fmt.Errorf("no rows to export")
	}
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	if err := g.writeDelimited(&buf, ',', rows, true); err != nil {
		return err
	}
	win := g.Window
	d := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		if wc == nil { // cancelled
			return
		}
		_, err = wc.Write(buf.Bytes())
		if cerr := wc.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			dialog.ShowError(err, win)
		}
	}, win)
	name := g.ExportFileName
	if name == "" {
		name = "export.csv"
	}
	d.SetFileName(name)
	d.Show()
	return nil
}

// ---------------------------------------------------------------------------
// Paste (right-click menu: Paste cell(s); Ctrl+V)
// ---------------------------------------------------------------------------

// canPaste reports whether a paste could change anything: the grid is
// editable and a cell is selected.
func (g *DataGrid) canPaste() bool {
	if g.Window == nil {
		return false
	}
	r := g.SelectedCellRange()
	for _, ci := range r.Cols {
		if g.isEditable(ci) {
			return len(r.Rows) > 0
		}
	}
	return false
}

// parseTSV splits clipboard text into rows of fields. It reads what
// CopySelectedCells writes (tabs between fields, line breaks between rows,
// double quotes around fields that need them) and also plain text from a
// spreadsheet. A single trailing line break is ignored.
func parseTSV(s string) [][]string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(strings.ReplaceAll(s, "\r", "\n"), "\n")
	if s == "" {
		return nil
	}
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = '\t'
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, err := r.ReadAll()
	if err != nil { // malformed quoting: fall back to a plain split
		lines := strings.Split(s, "\n")
		recs = make([][]string, len(lines))
		for i, l := range lines {
			recs[i] = strings.Split(l, "\t")
		}
	}
	return recs
}

// PasteCells pastes the clipboard text (tab-separated, as written by Ctrl+C /
// Copy cell) into the grid.
//
//   - One value and several selected cells: the value goes into every
//     selected cell.
//   - Otherwise the block starts at the top-left selected cell (the cursor
//     cell if there is no rectangle) and runs right and down in display
//     order. Whatever falls outside the grid is dropped.
//
// Read-only columns are skipped. Every value is parsed first, and if one is
// not valid for its column nothing is changed and the error is returned. A
// cell rejected by BeforeCellChange or the data source is left alone and the
// first such error is returned after the rest were pasted. OnCellChanged
// fires once per cell that really changed.
func (g *DataGrid) PasteCells() error {
	if g.Window == nil {
		return errNoWindow
	}
	if g.edit != nil { // the inline editor handles its own paste
		return nil
	}
	if !g.editable() || g.ReadOnly {
		return fmt.Errorf("the grid is read-only")
	}
	r := g.SelectedCellRange()
	if len(r.Rows) == 0 || len(r.Cols) == 0 {
		return fmt.Errorf("no cell selected")
	}
	recs := parseTSV(g.Window.Clipboard().Content())
	if len(recs) == 0 {
		return fmt.Errorf("the clipboard is empty")
	}

	type target struct {
		row, col int
		val      any
	}
	var targets []target
	add := func(ri, ci int, text string) error {
		if !g.isEditable(ci) {
			return nil
		}
		col := g.cols[ci]
		val, err := col.parse(text)
		if err != nil {
			return fmt.Errorf("%s: %w", col.Title, err)
		}
		targets = append(targets, target{ri, ci, val})
		return nil
	}

	if len(recs) == 1 && len(recs[0]) == 1 {
		for _, ri := range r.Rows {
			for _, ci := range r.Cols {
				if err := add(ri, ci, recs[0][0]); err != nil {
					return err
				}
			}
		}
	} else {
		g.ensureView()
		v0 := g.pos[r.Rows[0]]
		c0 := r.Cols[0]
		for i, rec := range recs {
			vr := v0 + i
			if vr >= len(g.view) {
				break
			}
			for j, text := range rec {
				ci := c0 + j
				if ci >= len(g.cols) {
					break
				}
				if err := add(g.view[vr], ci, text); err != nil {
					return err
				}
			}
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("nothing to paste: the target cells are read-only")
	}

	var evs []CellChange
	var firstErr error
	for _, t := range targets {
		col := g.cols[t.col]
		ev, changed, err := g.applyChange(t.row, t.col, g.src.Value(t.row, col.Key), t.val)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", col.Title, err)
			}
			continue
		}
		if changed {
			evs = append(evs, ev)
		}
	}
	g.redraw()
	if g.OnCellChanged != nil {
		for _, ev := range evs {
			g.OnCellChanged(ev)
		}
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// Cell widgets and custom shortcuts (see DataGrid.CellWidget, AddShortcut)
// ---------------------------------------------------------------------------

// cursorHasWidget reports whether the cursor cell currently shows a widget.
func (g *DataGrid) cursorHasWidget() bool {
	if g.CellWidget == nil || g.rend == nil {
		return false
	}
	vr := g.cursorViewRow()
	k := vr - g.topRow
	if vr < 0 || k < 0 || k >= len(g.rend.cellW) || g.curCol >= len(g.rend.cellW[k]) {
		return false
	}
	return g.rend.cellW[k][g.curCol] != nil
}

// AddShortcut registers fn for a keyboard shortcut while the grid has focus
// (and no cell editor is open). idx is the cursor row in YOUR data (-1 if
// none), key the cursor column's Key, row a snapshot of that row. A second
// call with the same shortcut replaces the first. fn is ignored if it is nil.
// It runs before the grid's own shortcuts, so it can override Ctrl+C etc.
//
//	g.AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyN, Modifier: fyne.KeyModifierControl},
//		func(idx int, key string, row fynex.Row) { addInvoice() })
func (g *DataGrid) AddShortcut(s fyne.Shortcut, fn func(idx int, key string, row Row)) {
	if g.shortcuts == nil {
		g.shortcuts = map[string]func(int, string, Row){}
	}
	if fn == nil {
		delete(g.shortcuts, s.ShortcutName())
		return
	}
	g.shortcuts[s.ShortcutName()] = fn
}

// RemoveShortcut unregisters a shortcut added with AddShortcut.
func (g *DataGrid) RemoveShortcut(s fyne.Shortcut) { delete(g.shortcuts, s.ShortcutName()) }
