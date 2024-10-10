package view

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View ...
type View struct {
	App       *tview.Application
	Frame     *tview.Frame
	Pages     *tview.Pages
	List      *tview.List
	Details   *tview.TextView
	NodeList  *tview.List
	ModalEdit func(p tview.Primitive, width, height int) tview.Primitive
}

// NewView ...
func NewView() *View {
	app := tview.NewApplication()
	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitle("Keys").
		SetTitleAlign(tview.AlignLeft)

	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	tv.SetBorder(true).
		SetTitle("Details").
		SetTitleAlign(tview.AlignLeft)

	nodeList := tview.NewList().
		ShowSecondaryText(false)
	nodeList.SetBorder(true).
		SetTitle("Nodes").
		SetTitleAlign(tview.AlignLeft)

	main := tview.NewFlex()
	main.AddItem(list, 0, 2, true)

	//
	left := tview.NewFlex().SetDirection(tview.FlexRow)
	left.AddItem(tv, 0, 1, false)
	left.AddItem(nodeList, 0, 2, false)

	main.AddItem(left, 0, 3, false)

	pages := tview.NewPages().
		AddPage("main", main, true, true)

	modal := func(p tview.Primitive, width, height int) tview.Primitive {
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 0, 1, false).
				AddItem(p, height, 1, true).
				AddItem(nil, 0, 1, false), width, 1, true).
			AddItem(nil, 0, 1, false)
	}

	frame := tview.NewFrame(pages)
	frame.AddText("[::b][c[][::-] Create key [::b][d[][::-] Delete key [::b][Ctrl+q][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
		tv,
		nodeList,
		modal,
	}

	return &v
}

func (v *View) NewCreateForm(header string) *tview.Form {
	form := tview.NewForm().
		AddInputField("Key", "", 30, nil, nil).
		AddInputField("Value", "", 30, nil, nil)
	form.SetBorder(true)
	form.SetTitle(header)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

func (v *View) NewDeleteQ(header string) *tview.Modal {
	deleteQ := tview.NewModal()
	deleteQ.SetText(fmt.Sprintf("Delete %s ?", header)).AddButtons([]string{"ok", "cancel"})
	return deleteQ
}
