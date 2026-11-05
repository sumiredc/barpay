package main

import "testing"

// A name is drawn wherever the member is shown, so a character that changes how it or the text
// around it is drawn is refused, as is a name that is not text at all.
func TestANameWithControlOrFormatCharactersIsRefused(t *testing.T) {
	for _, name := range []string{
		"Taro\u202Egpj.exe", // a bidi override
		"Ta\u200Bro",        // a zero-width space
		"Taro\nHanako",      // a newline
		"Taro\t",            // a tab
		"Taro\x00",          // a NUL
		"Taro\xff",          // not UTF-8
	} {
		if validName(name) {
			t.Errorf("validName(%q) = true, want false", name)
		}
	}
	for _, name := range []string{"Taro", "山田 太郎", "José", "O'Brien"} {
		if !validName(name) {
			t.Errorf("validName(%q) = false, want true", name)
		}
	}
}
