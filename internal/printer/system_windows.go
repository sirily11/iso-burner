package printer

func systemService() Service {
	return &Native{ListPrinters: listWindowsPrinters, Print: printWindowsDocument, Interfaces: windowsLANInterfaces}
}
