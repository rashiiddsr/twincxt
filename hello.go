package main

// Windows Hello for the app lock, through the WinRT UserConsentVerifier:
// face, fingerprint or the Windows PIN, in the system's own dialog, owned by
// the Twincxt window. It is an extra way to unlock; the Twincxt PIN or
// password stays as the fallback (and for "Forgot").

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	combase                 = windows.NewLazySystemDLL("combase.dll")
	pRoGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	pWindowsCreateString    = combase.NewProc("WindowsCreateString")
	pWindowsDeleteString    = combase.NewProc("WindowsDeleteString")

	iidConsentStatics = windows.GUID{Data1: 0xAF4F3F91, Data2: 0x564C, Data3: 0x4DDC, Data4: [8]byte{0xB8, 0xB5, 0x97, 0x34, 0x47, 0x62, 0x7C, 0x65}}
	iidConsentInterop = windows.GUID{Data1: 0x39E050C3, Data2: 0x4E74, Data3: 0x441A, Data4: [8]byte{0x8D, 0xC0, 0xB8, 0x11, 0x04, 0xDF, 0x94, 0x9C}}
	// IAsyncOperation<UserConsentVerificationResult>
	iidAsyncConsentResult = windows.GUID{Data1: 0xFD596FFD, Data2: 0x2318, Data3: 0x558F, Data4: [8]byte{0x9D, 0xBE, 0xD2, 0x1D, 0xF4, 0x37, 0x64, 0xA5}}
	iidAsyncInfo          = windows.GUID{Data1: 0x00000036, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

const consentClass = "Windows.Security.Credentials.UI.UserConsentVerifier"

func hstring(s string) uintptr {
	u := windows.StringToUTF16(s)
	var h uintptr
	pWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h)))
	return h
}

func consentFactory(iid *windows.GUID) uintptr {
	if pRoGetActivationFactory.Find() != nil {
		return 0
	}
	name := hstring(consentClass)
	defer pWindowsDeleteString.Call(name)
	var f uintptr
	if r, _, _ := pRoGetActivationFactory.Call(name, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&f))); r != 0 {
		return 0
	}
	return f
}

// waitAsync polls a WinRT async operation on the UI thread and calls done
// with its result (an enum) once it has completed; -1 when it failed.
func (a *app) waitAsync(op uintptr, done func(int32)) {
	var info uintptr
	if comCall(op, 0, uintptr(unsafe.Pointer(&iidAsyncInfo)), uintptr(unsafe.Pointer(&info))) != 0 {
		comRelease(op)
		done(-1)
		return
	}
	var poll func()
	poll = func() {
		switch comGetInt(info, 7) { // IAsyncInfo::get_Status
		case 0: // Started
			time.AfterFunc(120*time.Millisecond, func() { a.ui(poll) })
		case 1: // Completed
			var result int32 = -1
			comCall(op, 8, uintptr(unsafe.Pointer(&result))) // IAsyncOperation::GetResults
			comRelease(info)
			comRelease(op)
			done(result)
		default: // Canceled or Error
			comRelease(info)
			comRelease(op)
			done(-1)
		}
	}
	poll()
}

// checkHello finds out once whether Windows Hello can be used.
func (a *app) checkHello() {
	f := consentFactory(&iidConsentStatics)
	if f == 0 {
		debugLog("hello: not available (no factory)")
		return
	}
	var op uintptr
	r := comCall(f, 6, uintptr(unsafe.Pointer(&op))) // CheckAvailabilityAsync
	comRelease(f)
	if r != 0 || op == 0 {
		return
	}
	a.waitAsync(op, func(v int32) {
		a.helloAvailable = v == 0 // UserConsentVerifierAvailability.Available
		debugLog("hello: availability " + itoa(int64(v)))
		a.pushState()
		a.autoHello()
	})
}

// startHello shows the Windows Hello dialog and unlocks Twincxt when it
// succeeds.
func (a *app) startHello() {
	if !a.appLocked || !a.cfg.LockBiometric || !a.helloAvailable || a.helloBusy {
		return
	}
	f := consentFactory(&iidConsentInterop)
	if f == 0 {
		return
	}
	msg := hstring("Unlock Twincxt")
	var op uintptr
	r := comCall(f, 6, a.hwnd, msg, uintptr(unsafe.Pointer(&iidAsyncConsentResult)), uintptr(unsafe.Pointer(&op))) // RequestVerificationForWindowAsync
	pWindowsDeleteString.Call(msg)
	comRelease(f)
	if r != 0 || op == 0 {
		debugLog("hello: request failed")
		return
	}
	a.helloBusy = true
	a.waitAsync(op, func(v int32) {
		a.helloBusy = false
		debugLog("hello: result " + itoa(int64(v)))
		switch v {
		case 0: // Verified
			if a.appLocked {
				a.cfg.LockFails, a.cfg.LockUntil = 0, 0
				a.cfg.save()
				a.unlockApp()
			}
		case 6, -1: // Canceled (or the dialog went away): stay on the lock screen
			a.focusActive()
		default:
			a.shell.Eval(`twincxt.lockResult({op:"unlock",ok:false,error:"Windows Hello isn't available right now. Use your ` + a.lockName() + `."})`)
			a.focusActive()
		}
	})
}

// autoHello opens Windows Hello by itself once per lock, when the locked
// window is in front.
func (a *app) autoHello() {
	if a.helloTried || !a.appLocked || !a.cfg.LockBiometric || !a.helloAvailable || !a.shellOK {
		return
	}
	if fg, _, _ := pGetForegroundWindow.Call(); fg != a.hwnd {
		return
	}
	a.helloTried = true
	a.startHello()
}
