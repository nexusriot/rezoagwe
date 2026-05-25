package view

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Border colors for focus highlighting.
var (
	BorderUnfocused = tcell.ColorDimGray
	BorderFocused   = tcell.ColorYellow
)

// View ...
type View struct {
	App       *tview.Application
	Frame     *tview.Frame
	Pages     *tview.Pages
	List      *tview.List
	Filter    *tview.InputField
	Details   *tview.TextView
	NodeList  *tview.List
	Chat      *tview.TextView
	ChatInput *tview.InputField
	Status    *tview.TextView
	ModalEdit func(p tview.Primitive, width, height int) tview.Primitive
}

// Focusables returns the panes that participate in Tab cycling, in order.
func (v *View) Focusables() []tview.Primitive {
	return []tview.Primitive{v.List, v.NodeList, v.Chat, v.ChatInput}
}

type borderable interface {
	SetBorderColor(tcell.Color) *tview.Box
}

// HighlightFocus colors the borders so the focused pane stands out.
func (v *View) HighlightFocus() {
	focused := v.App.GetFocus()
	for _, p := range v.Focusables() {
		if b, ok := p.(borderable); ok {
			if p == focused {
				b.SetBorderColor(BorderFocused)
			} else {
				b.SetBorderColor(BorderUnfocused)
			}
		}
	}
	// Filter shares the keys-pane focus group: highlight when it has focus.
	if v.App.GetFocus() == v.Filter {
		v.Filter.SetBorderColor(BorderFocused)
	} else {
		v.Filter.SetBorderColor(BorderUnfocused)
	}
}

// NewView ...
func NewView() *View {
	app := tview.NewApplication()

	list := tview.NewList().
		ShowSecondaryText(true).
		SetSecondaryTextColor(tcell.ColorDarkGray).
		SetSelectedBackgroundColor(tcell.ColorDarkSlateGray)
	list.SetBorder(true).SetTitle("Keys").SetTitleAlign(tview.AlignLeft).
		SetBorderColor(BorderUnfocused)

	filter := tview.NewInputField().
		SetLabel(" / ").
		SetFieldWidth(0)
	filter.SetBorder(true).
		SetTitle("Filter (Esc to clear)").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(BorderUnfocused)

	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	tv.SetBorder(true).SetTitle("Details").SetTitleAlign(tview.AlignLeft).
		SetBorderColor(BorderUnfocused)

	nodeList := tview.NewList().ShowSecondaryText(false)
	nodeList.SetBorder(true).SetTitle("Nodes").SetTitleAlign(tview.AlignLeft).
		SetBorderColor(BorderUnfocused)

	chat := tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	chat.SetBorder(true).SetTitle("Chat").SetTitleAlign(tview.AlignLeft).
		SetBorderColor(BorderUnfocused)

	chatInput := tview.NewInputField().
		SetLabel(" > ").
		SetFieldWidth(0)
	chatInput.SetBorder(true).
		SetTitle("Message (Enter to send, Tab to switch focus)").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(BorderUnfocused)

	status := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft).
		SetChangedFunc(func() {
			app.Draw()
		})

	left := tview.NewFlex().SetDirection(tview.FlexRow)
	left.AddItem(filter, 3, 0, false)
	left.AddItem(list, 0, 1, true)

	right := tview.NewFlex().SetDirection(tview.FlexRow)
	right.AddItem(tv, 0, 2, false)
	right.AddItem(nodeList, 0, 3, false)
	right.AddItem(chat, 0, 5, false)

	top := tview.NewFlex()
	top.AddItem(left, 0, 2, true)
	top.AddItem(right, 0, 3, false)

	outer := tview.NewFlex().SetDirection(tview.FlexRow)
	outer.AddItem(top, 0, 1, true)
	outer.AddItem(chatInput, 3, 0, false)
	outer.AddItem(status, 1, 0, false)

	pages := tview.NewPages().AddPage("main", outer, true, true)

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
	frame.AddText(
		"[::b][c][::-] Create  [::b][e/↵][::-] Edit  [::b][d][::-] Delete  [::b][/][::-] Filter  [::b][Tab][::-] Cycle  [::b][Ctrl+Q][::-] Quit",
		false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
		filter,
		tv,
		nodeList,
		chat,
		chatInput,
		status,
		modal,
	}

	return &v
}

// NewKeyForm builds the create/edit form. If keyReadOnly is true, the key
// field is shown but cannot be modified (used for editing existing keys).
func (v *View) NewKeyForm(title, key, value string, keyReadOnly bool) (*tview.Form, *tview.InputField, *tview.TextArea) {
	keyField := tview.NewInputField().SetLabel("Key").SetFieldWidth(40).SetText(key)
	if keyReadOnly {
		keyField.SetFieldTextColor(tcell.ColorDarkGray)
		keyField.SetDisabled(true)
	}

	valueArea := tview.NewTextArea().SetLabel("Value").SetSize(6, 40).SetText(value, true)

	form := tview.NewForm().
		AddFormItem(keyField).
		AddFormItem(valueArea)
	form.SetBorder(true)
	form.SetTitle(title)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form, keyField, valueArea
}

func (v *View) NewDeleteQ(header string) *tview.Modal {
	deleteQ := tview.NewModal()
	deleteQ.SetText(fmt.Sprintf("Delete %s ?", header)).AddButtons([]string{"ok", "cancel"})
	return deleteQ
}
