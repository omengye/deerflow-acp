package secrets

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Run the real modal message loop on a private Windows desktop, without
// stealing focus or sending keystrokes to any interactive application.
func TestNativeCredentialDialogSaveAndCancel(t *testing.T) {
	for _, save := range []bool{true, false} {
		name, _ := windows.UTF16PtrFromString(fmt.Sprintf("DeerFlow-test-%d", time.Now().UnixNano()))
		desk, _, err := user32.NewProc("CreateDesktopW").Call(uintptr(unsafe.Pointer(name)), 0, 0, 0, 0x1ff, 0)
		if desk == 0 {
			t.Fatalf("create test desktop: %v", err)
		}
		func() {
			defer user32.NewProc("CloseDesktop").Call(desk)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			type answer struct {
				p CredentialPair
				e error
			}
			done := make(chan answer, 1)
			tid := make(chan uint32, 1)
			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				thread := windows.GetCurrentThreadId()
				old, _, _ := user32.NewProc("GetThreadDesktop").Call(uintptr(thread))
				ok, _, err := user32.NewProc("SetThreadDesktop").Call(desk)
				if ok == 0 {
					done <- answer{e: err}
					return
				}
				defer user32.NewProc("SetThreadDesktop").Call(old)
				tid <- thread
				p, e := DesktopCredentialPrompt(ctx, CredentialScope{Consumer: "skill:fixture", Service: "https://example.test", Account: "default"}, BothFields)
				done <- answer{p, e}
			}()
			var thread uint32
			select {
			case a := <-done:
				t.Fatal("test desktop unavailable", a.e)
			case thread = <-tid:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var wnd uintptr
			enumerate := windows.NewCallback(func(h, w uintptr) uintptr {
				child, _, _ := user32.NewProc("GetDlgItem").Call(h, formKey)
				if child != 0 {
					wnd = h
					return 0
				}
				return 1
			})
			for wnd == 0 {
				user32.NewProc("EnumThreadWindows").Call(uintptr(thread), enumerate, 0)
				select {
				case a := <-done:
					t.Fatal("form ended before input", a.e)
				case <-ctx.Done():
					t.Fatal("form not found")
				default:
				}
				if wnd == 0 {
					time.Sleep(10 * time.Millisecond)
				}
			}
			char, _, _ := sendDialogItem.Call(wnd, formKey, 0xd2, 0, 0)
			if char == 0 {
				t.Fatal("key field is unmasked")
			}
			if save {
				for id, s := range map[uintptr]string{formUser: "demo-user", formKey: "synthetic-凭据-123"} {
					v, _ := windows.UTF16PtrFromString(s)
					setDialogText.Call(wnd, id, uintptr(unsafe.Pointer(v)))
				}
				postDialogMessage.Call(wnd, 0x111, 1, 0)
			} else {
				cancel()
			}
			select {
			case a := <-done:
				defer a.p.Clear()
				if save && (a.e != nil || string(a.p.Username) != "demo-user" || string(a.p.Key) != "synthetic-凭据-123") || !save && (!errors.Is(a.e, context.Canceled) || len(a.p.Key) != 0) {
					t.Fatal("wrong form result", a.e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("native form did not close")
			}
		}()
	}
}
