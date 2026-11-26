package main

// Downloads: where files go (the Downloads folder unless you pick another),
// and the system's folder picker and Save As dialog (IFileDialog).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")

	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog   = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	clsidFileSaveDialog = windows.GUID{Data1: 0xC0B4E2F3, Data2: 0xBA21, Data3: 0x4773, Data4: [8]byte{0x8D, 0xBA, 0x33, 0x5E, 0xC9, 0x46, 0xEB, 0x8B}}
	iidFileSaveDialog   = windows.GUID{Data1: 0x84BCCD23, Data2: 0x5FDE, Data3: 0x4CDB, Data4: [8]byte{0xAE, 0xA4, 0xAF, 0x64, 0xB8, 0x3D, 0x78, 0xAB}}
	iidShellItem        = windows.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

const (
	fosOverwritePrompt  = 0x2
	fosPickFolders      = 0x20
	fosForceFileSystem  = 0x40
	fosPathMustExist    = 0x800
	sigdnFileSysPath    = 0x80058000
	idxShow             = 3
	idxSetOptions       = 9
	idxGetOptions       = 10
	idxSetFolder        = 12
	idxSetFileName      = 15
	idxSetTitle         = 17
	idxGetResult        = 20
	idxSetDefaultExt    = 22
	idxItemDisplayName  = 5
	downloadsFolderName = "Downloads"
)

// systemDownloads is the user's Downloads folder.
func systemDownloads() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0); err == nil && p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, downloadsFolderName)
}

// downloadDir is where downloads go: the chosen folder if it still exists,
// otherwise the Downloads folder.
func (a *app) downloadDir() string {
	if d := a.cfg.DownloadDir; d != "" {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
	}
	return systemDownloads()
}

// uniquePath returns dir\name, or dir\name (1).ext, (2)… if it exists.
func uniquePath(dir, name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		name = "download"
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	p := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, base+" ("+strconv.Itoa(i)+")"+ext)
	}
}

func shellItem(path string) uintptr {
	var item uintptr
	pSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(utf16ptr(path))), 0,
		uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item)))
	return item
}

// runFileDialog shows an IFileDialog (open-folder or save) and returns the
// chosen path; ok is false when it was cancelled.
func runFileDialog(owner uintptr, save bool, title, folder, name string) (string, bool) {
	clsid, iid := &clsidFileOpenDialog, &iidFileOpenDialog
	if save {
		clsid, iid = &clsidFileSaveDialog, &iidFileSaveDialog
	}
	var dlg uintptr
	if r, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(clsid)), 0, 1, /*CLSCTX_INPROC_SERVER*/
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&dlg))); r != 0 || dlg == 0 {
		return "", false
	}
	defer comRelease(dlg)
	var opts uint32
	comCall(dlg, idxGetOptions, uintptr(unsafe.Pointer(&opts)))
	if save {
		opts |= fosOverwritePrompt | fosForceFileSystem | fosPathMustExist
	} else {
		opts |= fosPickFolders | fosForceFileSystem | fosPathMustExist
	}
	comCall(dlg, idxSetOptions, uintptr(opts))
	comCall(dlg, idxSetTitle, uintptr(unsafe.Pointer(utf16ptr(title))))
	if folder != "" {
		if item := shellItem(folder); item != 0 {
			comCall(dlg, idxSetFolder, item)
			comRelease(item)
		}
	}
	if save && name != "" {
		comCall(dlg, idxSetFileName, uintptr(unsafe.Pointer(utf16ptr(name))))
		if ext := strings.TrimPrefix(filepath.Ext(name), "."); ext != "" {
			comCall(dlg, idxSetDefaultExt, uintptr(unsafe.Pointer(utf16ptr(ext))))
		}
	}
	if comCall(dlg, idxShow, owner) != 0 {
		return "", false // cancelled (or failed)
	}
	var item uintptr
	if comCall(dlg, idxGetResult, uintptr(unsafe.Pointer(&item))) != 0 || item == 0 {
		return "", false
	}
	defer comRelease(item)
	var p uintptr
	if comCall(item, idxItemDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&p))) != 0 {
		return "", false
	}
	path := coTaskString(p)
	return path, path != ""
}

// onDownloadMessage handles the shell's "download" messages: "choose" picks a
// folder, "reset" goes back to the Downloads folder.
func (a *app) onDownloadMessage(cmd string) {
	switch cmd {
	case "choose":
		if dir, ok := runFileDialog(a.hwnd, false, "Save downloads to", a.downloadDir(), ""); ok {
			a.cfg.DownloadDir = dir
			if strings.EqualFold(filepath.Clean(dir), filepath.Clean(systemDownloads())) {
				a.cfg.DownloadDir = ""
			}
		}
	case "reset":
		a.cfg.DownloadDir = ""
	}
	a.cfg.save()
	a.pushState()
}

// startDownload decides where a download goes. finish gets the file path, or
// cancel when you closed the Save As dialog.
func (a *app) startDownload(svcID, suggested string, finish func(path string, cancel bool)) {
	name := filepath.Base(suggested)
	notify := func(p string) {
		svcName := "Twincxt"
		if s := a.svc(svcID); s != nil {
			svcName = s.name
		}
		watchDownload(p, svcName, serviceIconPath(svcID))
	}
	if !a.cfg.DownloadAsk {
		p := uniquePath(a.downloadDir(), name)
		finish(p, false)
		notify(p)
		return
	}
	// Ask: the Save As dialog runs after the event returns (deferral).
	a.ui(func() {
		p, ok := runFileDialog(a.hwnd, true, "Save "+name, a.downloadDir(), name)
		if !ok {
			finish("", true)
			return
		}
		finish(p, false)
		notify(p)
	})
}
