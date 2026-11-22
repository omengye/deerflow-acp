package secrets

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	dialogBox         = user32.NewProc("DialogBoxIndirectParamW")
	endDialog         = user32.NewProc("EndDialog")
	setDialogText     = user32.NewProc("SetDlgItemTextW")
	getDialogText     = user32.NewProc("GetDlgItemTextW")
	sendDialogItem    = user32.NewProc("SendDlgItemMessageW")
	postDialogMessage = user32.NewProc("PostMessageW")
	setUserData       = user32.NewProc("SetWindowLongPtrW")
	getUserData       = user32.NewProc("GetWindowLongPtrW")
	setDialogPosition = user32.NewProc("SetWindowPos")
	activeForms       sync.Map
	nextForm          atomic.Uintptr
	formCallback      = windows.NewCallback(credentialDialogProc)
)

const (
	formUser          = 101
	formKey           = 102
	formStatus        = 103
	formCancelMessage = 0x8000 + 23
)

type credentialForm struct {
	ctx    context.Context
	pair   CredentialPair
	fields CredentialFields
	stop   func() bool
}

// Standard Windows dialog template. The dialog manager supplies keyboard
// navigation, default/cancel buttons, password masking and DPI-aware font layout.
func credentialDialogTemplate(scope CredentialScope, fields CredentialFields) []byte {
	b := make([]byte, 0, 1024)
	word := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	dword := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	str := func(s string) {
		v, _ := windows.UTF16FromString(s)
		for _, c := range v {
			word(c)
		}
	}
	dword(0x90C808C0)
	dword(0)
	word(uint16(4 + 2*len(fields.labels())))
	for _, n := range []uint16{0, 0, 345, 146} {
		word(n)
	}
	word(0)
	word(0)
	str("DeerFlow · 保存服务凭据")
	word(9)
	str("Segoe UI")
	item := func(id, cls, x, y, w, h uint16, style uint32, title string) {
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		dword(style | 0x50000000)
		dword(0)
		for _, n := range []uint16{x, y, w, h, id, 0xffff, cls} {
			word(n)
		}
		str(title)
		word(0)
	}
	item(100, 0x82, 8, 6, 329, 43, 0x80, "调用方  "+scope.Consumer+"\r\n服务  "+scope.Service+"\r\n账号别名  "+scope.Account)
	y := uint16(51)
	if fields&UsernameField != 0 {
		item(104, 0x82, 8, y+2, 56, 12, 0, "用户名(&U)")
		item(formUser, 0x81, 72, y, 262, 15, 0x00810080, "")
		y += 22
	}
	if fields&KeyField != 0 {
		item(105, 0x82, 8, y+2, 62, 12, 0, "密钥或密码(&K)")
		item(formKey, 0x81, 72, y, 262, 15, 0x008100A0, "")
	}
	item(formStatus, 0x82, 8, 96, 329, 17, 0x80, "凭据保存在本机，后续调用按工具的审批设置使用。")
	item(1, 0x80, 179, 121, 82, 16, 0x10001, "保存并使用")
	item(2, 0x80, 268, 121, 66, 16, 0x10000, "取消")
	return b
}

func readFormField(hwnd, id uintptr) []byte {
	buf := make([]uint16, MaxStringBytes+1)
	defer clear(buf)
	n, _, _ := getDialogText.Call(hwnd, id, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return nil
	}
	out := make([]byte, 0, n)
	for i := 0; i < int(n); i++ {
		r := rune(buf[i])
		if utf16.IsSurrogate(r) && i+1 < int(n) {
			r = utf16.DecodeRune(r, rune(buf[i+1]))
			i++
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

func credentialDialogProc(hwnd, msg, w, l uintptr) uintptr {
	const userdata = ^uintptr(20) // GWLP_USERDATA (-21)
	if msg == 0x110 {             // WM_INITDIALOG
		x, ok := activeForms.Load(l)
		if !ok {
			endDialog.Call(hwnd, 2)
			return 1
		}
		f := x.(*credentialForm)
		setUserData.Call(hwnd, userdata, l)
		for _, id := range []uintptr{formUser, formKey} {
			sendDialogItem.Call(hwnd, id, 0xc5, MaxStringBytes, 0)
		}
		setDialogPosition.Call(hwnd, ^uintptr(0), 0, 0, 0, 0, 0x13)
		f.stop = context.AfterFunc(f.ctx, func() { postDialogMessage.Call(hwnd, formCancelMessage, l, 0) })
		return 1
	}
	id, _, _ := getUserData.Call(hwnd, userdata)
	x, ok := activeForms.Load(id)
	if !ok {
		return 0
	}
	f := x.(*credentialForm)
	switch {
	case msg == 0x111 && w&0xffff == 1:
		pair := CredentialPair{readFormField(hwnd, formUser), readFormField(hwnd, formKey)}
		if f.fields&UsernameField != 0 && validateValue(KindString, pair.Username) != nil || f.fields&KeyField != 0 && validateValue(KindString, pair.Key) != nil {
			pair.Clear()
			text, _ := windows.UTF16PtrFromString("请填好表单中的每一项，每项最多 64 KiB。")
			setDialogText.Call(hwnd, formStatus, uintptr(unsafe.Pointer(text)))
			return 1
		}
		f.pair = pair
		endDialog.Call(hwnd, 1)
	case msg == 0x10 || msg == formCancelMessage && w == id || msg == 0x111 && w&0xffff == 2:
		endDialog.Call(hwnd, 2)
	default:
		return 0
	}
	return 1
}

// DesktopCredentialPrompt runs on the tool worker, outside the desktop UI
// thread. Plaintext never crosses ACP, a shell, or any process boundary.
func DesktopCredentialPrompt(ctx context.Context, scope CredentialScope, fields CredentialFields) (CredentialPair, error) {
	if err := ctx.Err(); err != nil {
		return CredentialPair{}, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	id := nextForm.Add(1)
	f := &credentialForm{ctx: ctx, fields: fields}
	activeForms.Store(id, f)
	defer activeForms.Delete(id)
	tmpl := credentialDialogTemplate(scope, fields)
	r, _, _ := dialogBox.Call(0, uintptr(unsafe.Pointer(&tmpl[0])), 0, formCallback, id)
	runtime.KeepAlive(tmpl)
	if f.stop != nil {
		f.stop()
	}
	if ctx.Err() != nil {
		f.pair.Clear()
		return CredentialPair{}, ctx.Err()
	}
	if r != 1 {
		f.pair.Clear()
		if r == 2 {
			return CredentialPair{}, ErrCredentialCancelled
		}
		return CredentialPair{}, errors.New("could not display the local credential form")
	}
	return f.pair, nil
}
