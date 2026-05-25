package view

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View ...
type View struct {
	App    *tview.Application
	Frame  *tview.Frame
	Pages  *tview.Pages
	List   *tview.List
	Status *tview.TextView
}

// NewView ...
func NewView() *View {
	app := tview.NewApplication()
	list := tview.NewList().
		ShowSecondaryText(true).
		SetSecondaryTextColor(tcell.ColorDarkGray)
	list.SetBorder(true).
		SetTitle("Nodes").
		SetTitleAlign(tview.AlignLeft)

	status := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft).
		SetChangedFunc(func() {
			app.Draw()
		})

	main := tview.NewFlex().SetDirection(tview.FlexRow)
	main.AddItem(list, 0, 1, true)
	main.AddItem(status, 1, 0, false)

	pages := tview.NewPages().
		AddPage("main", main, true, true)

	frame := tview.NewFrame(pages)
	frame.AddText("[::b][Ctrl+q][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
		status,
	}

	return &v
}
