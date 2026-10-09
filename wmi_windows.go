//go:build windows

package main

// A minimal WMI query client using raw COM calls, so no third-party packages
// are needed. It supports exactly what this program uses: running a WQL
// query and reading integer and string properties.

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	ole32                    = syscall.NewLazyDLL("ole32.dll")
	oleaut32                 = syscall.NewLazyDLL("oleaut32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
	procCoInitializeSecurity = ole32.NewProc("CoInitializeSecurity")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")
	procCoSetProxyBlanket    = ole32.NewProc("CoSetProxyBlanket")
	procSysAllocString       = oleaut32.NewProc("SysAllocString")
	procSysFreeString        = oleaut32.NewProc("SysFreeString")
	procVariantClear         = oleaut32.NewProc("VariantClear")

	securityOnce sync.Once
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidWbemLocator = guid{0x4590f811, 0x1d3a, 0x11d0, [8]byte{0x89, 0x1f, 0x00, 0xaa, 0x00, 0x4b, 0x2e, 0x24}}
	iidIWbemLocator  = guid{0xdc12a687, 0x737f, 0x11cf, [8]byte{0x88, 0x4d, 0x00, 0xaa, 0x00, 0x4b, 0x2e, 0x24}}
)

const (
	coinitMultithreaded      = 0x0
	clsctxInprocServer       = 0x1
	rpcCAuthnLevelDefault    = 0
	rpcCAuthnLevelCall       = 3
	rpcCImpLevelImpersonate  = 3
	rpcCAuthnWinNT           = 10
	wbemFlagForwardOnly      = 0x20
	wbemFlagReturnImmediate  = 0x10
	wbemInfinite             = 0xffffffff
	sFalse                   = 1
	rpcEChangedMode          = 0x80010106
	vtI4, vtBSTR, vtUI4      = 3, 8, 19
	vtI2, vtUI1, vtUI2, vtI1 = 2, 17, 18, 16

	// vtable slots
	slotRelease       = 2
	slotConnectServer = 3  // IWbemLocator
	slotExecQuery     = 20 // IWbemServices
	slotNext          = 4  // IEnumWbemClassObject
	slotGet           = 4  // IWbemClassObject
)

type variant struct {
	vt      uint16
	_, _, _ uint16
	val     unsafe.Pointer
	_       uintptr
}

// call invokes a COM method by vtable slot.
func call(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, slot*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		call(obj, slotRelease)
	}
}

func bstr(s string) uintptr {
	p, _ := syscall.UTF16PtrFromString(s)
	r, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(p)))
	return r
}

func freeBSTR(b uintptr) { procSysFreeString.Call(b) }

func failed(hr uintptr) bool { return int32(hr) < 0 }

// wmiQuery runs a WQL query in a namespace and returns the requested
// properties per row (int64 or string; nil when absent).
func wmiQuery(namespace, query string, props []string) ([]map[string]any, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if failed(hr) && hr != rpcEChangedMode {
		return nil, fmt.Errorf("CoInitializeEx: 0x%08x", uint32(hr))
	}
	if !failed(hr) {
		defer procCoUninitialize.Call()
	}
	securityOnce.Do(func() {
		// May fail with RPC_E_TOO_LATE if already set; that is fine.
		procCoInitializeSecurity.Call(0, ^uintptr(0), 0, 0, rpcCAuthnLevelDefault, rpcCImpLevelImpersonate, 0, 0, 0)
	})

	var locator unsafe.Pointer
	hr, _, _ = procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidWbemLocator)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIWbemLocator)), uintptr(unsafe.Pointer(&locator)))
	if failed(hr) {
		return nil, fmt.Errorf("WMI unavailable (CoCreateInstance: 0x%08x)", uint32(hr))
	}
	defer release(locator)

	ns := bstr(namespace)
	defer freeBSTR(ns)
	var services unsafe.Pointer
	hr = call(locator, slotConnectServer, ns, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&services)))
	if failed(hr) {
		return nil, fmt.Errorf("WMI connect to %s: 0x%08x", namespace, uint32(hr))
	}
	defer release(services)
	procCoSetProxyBlanket.Call(uintptr(services), rpcCAuthnWinNT, 0, 0, rpcCAuthnLevelCall, rpcCImpLevelImpersonate, 0, 0)

	lang, q := bstr("WQL"), bstr(query)
	defer freeBSTR(lang)
	defer freeBSTR(q)
	var enum unsafe.Pointer
	hr = call(services, slotExecQuery, lang, q, wbemFlagForwardOnly|wbemFlagReturnImmediate, 0, uintptr(unsafe.Pointer(&enum)))
	if failed(hr) {
		return nil, fmt.Errorf("WMI query %q: 0x%08x", query, uint32(hr))
	}
	defer release(enum)

	var rows []map[string]any
	for {
		var obj unsafe.Pointer
		var n uint32
		hr = call(enum, slotNext, wbemInfinite, 1, uintptr(unsafe.Pointer(&obj)), uintptr(unsafe.Pointer(&n)))
		if failed(hr) {
			return rows, fmt.Errorf("WMI enumerate: 0x%08x", uint32(hr))
		}
		if n == 0 || hr == sFalse && obj == nil {
			return rows, nil
		}
		row := map[string]any{}
		for _, p := range props {
			row[p] = getProp(obj, p)
		}
		release(obj)
		rows = append(rows, row)
	}
}

func getProp(obj unsafe.Pointer, name string) any {
	p, _ := syscall.UTF16PtrFromString(name)
	var v variant
	if failed(call(obj, slotGet, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&v)), 0, 0)) {
		return nil
	}
	defer procVariantClear.Call(uintptr(unsafe.Pointer(&v)))
	raw := unsafe.Pointer(&v.val)
	switch v.vt {
	case vtI4:
		return int64(*(*int32)(raw))
	case vtUI4:
		return int64(*(*uint32)(raw))
	case vtI2:
		return int64(*(*int16)(raw))
	case vtUI2:
		return int64(*(*uint16)(raw))
	case vtI1:
		return int64(*(*int8)(raw))
	case vtUI1:
		return int64(*(*uint8)(raw))
	case vtBSTR:
		if v.val == nil {
			return ""
		}
		n := 0
		for *(*uint16)(unsafe.Add(v.val, n*2)) != 0 {
			n++
		}
		return syscall.UTF16ToString(unsafe.Slice((*uint16)(v.val), n))
	}
	return nil
}
