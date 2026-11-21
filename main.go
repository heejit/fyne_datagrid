package main

import (
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"github.com/shopspring/decimal"

)

type Task struct {
	ID     int64           `grid:"title=ID;width=6;readonly"`
	Name   string          `grid:"title=Task;width=34"`
	Amount decimal.Decimal `grid:"title=Amount;width=16;thousands"`
	Due    time.Time       `grid:"title=Due;width=14"`
	Done   bool            `grid:"title=Done;width=10"`   // drawn as a checkbox
	Action bool            `grid:"title=Action;width=14"` // drawn as a button
}

func main() {
	a := app.New()
	w := a.NewWindow("Datagrid + CellWidget demo")

	now := time.Now()
	tasks := []Task{
		{1, "Write report", decimal.NewFromInt(1250), now.AddDate(0, 0, 2), false, false},
		{2, "Pay invoice", decimal.NewFromFloat(-4800.5), now.AddDate(0, 0, 5), true, false},
		{3, "Call supplier", decimal.NewFromInt(300), now.AddDate(0, 0, 1), false, false},
		{4, "Review contract", decimal.NewFromInt(99000), now.AddDate(0, 0, 9), false, false},
	}
	for i := 5; i <= 40; i++ {
		tasks = append(tasks, Task{int64(i), fmt.Sprintf("Task %d", i),
			decimal.NewFromInt(int64(i * 100)), now.AddDate(0, 0, i), i%3 == 0, false})
	}

	status := widget.NewLabel("Ctrl+N add row | F5 reload | Ctrl+D done on/off | Ctrl+C copy")

	g := New(&tasks)
	g.SetReadOnly(false)

	// negative amounts in red
	g.StyleCell = func(row int, key string) (CellStyle, bool) {
		if key == "Amount" && row < len(tasks) && tasks[row].Amount.IsNegative() {
			return CellStyle{Bold: true}, true
		}
		return CellStyle{}, false
	}

	// ---- widget in a cell ----
	g.CellWidget = func(row int, key string, old fyne.CanvasObject) fyne.CanvasObject {
		if row < 0 || row >= len(tasks) {
			return nil
		}
		switch key {
		case "Done":
			chk, ok := old.(*widget.Check)
			if !ok {
				chk = widget.NewCheck("", nil)
			}
			chk.OnChanged = nil // SetChecked must not fire the handler
			chk.SetChecked(tasks[row].Done)
			chk.OnChanged = func(v bool) {
				tasks[row].Done = v
				status.SetText(fmt.Sprintf("Task %d done = %v", tasks[row].ID, v))
			}
			return chk
		case "Action":
			btn, ok := old.(*widget.Button)
			if !ok {
				btn = widget.NewButton("Open", nil)
			}
			btn.OnTapped = func() { // rebind every call: pooled cells get reused for other rows
				dialog.ShowInformation("Task", fmt.Sprintf("Opening %q", tasks[row].Name), w)
			}
			return btn
		}
		return nil
	}

	// ---- shortcuts ----
	g.AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyN, Modifier: fyne.KeyModifierControl},
		func(idx int, key string, row Row) {
			if err := g.AppendRow(); err != nil {
				status.SetText("Add failed: " + err.Error())
				return
			}
			status.SetText("Row added")
		})
	g.AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyD, Modifier: fyne.KeyModifierControl},
		func(idx int, key string, row Row) {
			if idx < 0 || idx >= len(tasks) {
				return
			}
			tasks[idx].Done = !tasks[idx].Done
			g.RepaintRow(idx)
			status.SetText(fmt.Sprintf("Task %d done = %v", tasks[idx].ID, tasks[idx].Done))
		})

	// plain key (no modifier)
	g.OnKey = func(k *fyne.KeyEvent, idx int, key string, row Row) bool {
		if k.Name == fyne.KeyF5 {
			g.Reload()
			status.SetText("Reloaded")
			return true
		}
		return false
	}

	w.SetContent(container.NewBorder(nil, status, nil, nil, g))
	w.Resize(fyne.NewSize(900, 520))
	w.Canvas().Focus(g)
	w.ShowAndRun()
}
