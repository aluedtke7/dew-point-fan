package display

// Interface definition for LCD and OLED
type Display interface {
	Backlight(on bool)
	Clear()
	ClearLine(ofs int)
	Close()
	GetCharsPerLine() int
	GetMinMaxRowNum() (int, int)
	PrintLine(line int, text string, scroll bool)
}
