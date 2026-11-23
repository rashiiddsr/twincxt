package main

// Unread badge on the taskbar button: a red dot with the number of unread
// chats (WhatsApp + Telegram), like an app badge on a phone. It is the
// taskbar's overlay icon (ITaskbarList3::SetOverlayIcon), drawn here.

import (
	"image"
	"image/png"
	"math"
	"os"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pCoCreateInstance    = ole32.NewProc("CoCreateInstance")
	pCreateCompatibleDC  = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection    = gdi32.NewProc("CreateDIBSection")
	pCreateBitmap        = gdi32.NewProc("CreateBitmap")
	pSelectObject        = gdi32.NewProc("SelectObject")
	pDeleteDC            = gdi32.NewProc("DeleteDC")
	pCreateFontW         = gdi32.NewProc("CreateFontW")
	pSetTextColor        = gdi32.NewProc("SetTextColor")
	pSetBkMode           = gdi32.NewProc("SetBkMode")
	pDrawTextW           = user32.NewProc("DrawTextW")
	pCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	pDestroyIcon         = user32.NewProc("DestroyIcon")
	clsidTaskbarList     = windows.GUID{Data1: 0x56FDF344, Data2: 0xFD6D, Data3: 0x11D0, Data4: [8]byte{0x95, 0x8A, 0x00, 0x60, 0x97, 0xC9, 0xA0, 0x90}}
	iidTaskbarList3      = windows.GUID{Data1: 0xEA1AFB91, Data2: 0x9E28, Data3: 0x4B86, Data4: [8]byte{0x90, 0xE9, 0x9E, 0x9F, 0x8A, 0x5E, 0xEF, 0xAF}}
	taskbarButtonCreated uint32
)

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPPM, YPPM    int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

func badgeText(n int) string {
	if n > 99 {
		return "99+"
	}
	return strconv.Itoa(n)
}

// badgeIcon draws a red dot (Windows' danger red) with the count in white.
func badgeIcon(n, size int) uintptr {
	text := badgeText(n)
	dc, _, _ := pCreateCompatibleDC.Call(0)
	if dc == 0 {
		return 0
	}
	defer pDeleteDC.Call(dc)
	bi := bitmapInfoHeader{Width: int32(size), Height: -int32(size), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits uintptr
	bmp, _, _ := pCreateDIBSection.Call(dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == 0 {
		return 0
	}
	px := unsafe.Slice((*uint32)(unsafe.Pointer(bits)), size*size)

	// 1. The number, white on black, anti-aliased by GDI: its brightness is
	//    the text's coverage.
	old, _, _ := pSelectObject.Call(dc, bmp)
	scale := 0.74
	if len(text) == 2 {
		scale = 0.62
	} else if len(text) > 2 {
		scale = 0.46
	}
	h := int32(math.Round(float64(size) * scale))
	font, _, _ := pCreateFontW.Call(uintptr(-h), 0, 0, 0, 700, 0, 0, 0, 1 /*DEFAULT_CHARSET*/, 0, 0,
		4 /*ANTIALIASED_QUALITY*/, 0, uintptr(unsafe.Pointer(utf16ptr("Segoe UI"))))
	oldFont, _, _ := pSelectObject.Call(dc, font)
	pSetTextColor.Call(dc, 0x00FFFFFF)
	pSetBkMode.Call(dc, 1 /*TRANSPARENT*/)
	r := rect{0, 0, int32(size), int32(size)}
	t16 := windows.StringToUTF16(text)
	pDrawTextW.Call(dc, uintptr(unsafe.Pointer(&t16[0])), uintptr(len(t16)-1), uintptr(unsafe.Pointer(&r)),
		0x1|0x4|0x20|0x100 /*CENTER|VCENTER|SINGLELINE|NOCLIP*/)
	pSelectObject.Call(dc, oldFont)
	pDeleteObject.Call(font)
	pSelectObject.Call(dc, old)

	// 2. The dot, anti-aliased by 4x4 supersampling, with the number on top.
	const red = 0xC42B1C
	rr, rg, rb := float64(red>>16&0xFF), float64(red>>8&0xFF), float64(red&0xFF)
	c := float64(size) / 2
	rad := c - 0.25
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			cover := 0.0
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					dx := float64(x) + (float64(sx)+0.5)/4 - c
					dy := float64(y) + (float64(sy)+0.5)/4 - c
					if dx*dx+dy*dy <= rad*rad {
						cover++
					}
				}
			}
			cover /= 16
			t := float64(px[y*size+x]>>8&0xFF) / 255 // text coverage
			cr := rr + (255-rr)*t
			cg := rg + (255-rg)*t
			cb := rb + (255-rb)*t
			a := cover
			if t > a {
				a = t
			}
			// Premultiplied BGRA.
			px[y*size+x] = uint32(math.Round(a*255))<<24 | uint32(math.Round(cr*a))<<16 |
				uint32(math.Round(cg*a))<<8 | uint32(math.Round(cb*a))
		}
	}

	if path := os.Getenv("TWINCXT_TEST_BADGE_PNG"); path != "" {
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		for i, v := range px {
			a := uint8(v >> 24)
			un := func(c uint32) uint8 {
				if a == 0 {
					return 0
				}
				return uint8(math.Min(255, float64(c&0xFF)*255/float64(a)))
			}
			img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = un(v>>16), un(v>>8), un(v), a
		}
		if f, err := os.Create(path + "-" + text + ".png"); err == nil {
			_ = png.Encode(f, img)
			f.Close()
		}
	}

	mask, _, _ := pCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, 0)
	ii := iconInfo{FIcon: 1, HbmMask: mask, HbmColor: bmp}
	icon, _, _ := pCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	pDeleteObject.Call(mask)
	pDeleteObject.Call(bmp)
	return icon
}

// updateBadge shows the total unread count on the taskbar button, or clears it.
func (a *app) updateBadge() {
	if a.hwnd == 0 {
		return
	}
	n := 0
	if a.cfg.Badge {
		for _, s := range a.svcs {
			n += s.unread
		}
	}
	if v, err := strconv.Atoi(os.Getenv("TWINCXT_TEST_BADGE")); err == nil {
		n = v // test hook: show this count
	}
	if n == a.badgeShown && a.taskbar != 0 {
		return
	}
	if a.taskbar == 0 {
		var tb uintptr
		r, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidTaskbarList)), 0, 1, /*CLSCTX_INPROC_SERVER*/
			uintptr(unsafe.Pointer(&iidTaskbarList3)), uintptr(unsafe.Pointer(&tb)))
		if r != 0 || tb == 0 {
			return
		}
		comCall(tb, 3) // HrInit
		a.taskbar = tb
	}
	var icon uintptr
	desc := ""
	if n > 0 {
		dpi := dpiOf(a.hwnd)
		size, _, _ := pGetSystemMetricsForDpi.Call(49 /*SM_CXSMICON*/, uintptr(dpi))
		if size < 16 {
			size = 16
		}
		icon = badgeIcon(n, int(size))
		desc = badgeText(n) + " unread"
	}
	var d uintptr
	if desc != "" {
		d = uintptr(unsafe.Pointer(utf16ptr(desc)))
	}
	if comCall(a.taskbar, 18, a.hwnd, icon, d) == 0 { // SetOverlayIcon
		a.badgeShown = n
	}
	if icon != 0 {
		pDestroyIcon.Call(icon)
	}
}
