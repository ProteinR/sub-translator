//go:build !darwin

package main

import "context"

const hasNativeWindow = false

// Вне macOS встроенного окна нет: панель открывается в обычном браузере,
// а запуск перевода сообщает, что нужен macOS.
type noEngine struct{}

func defaultEngine() tabEngine { return noEngine{} }

func runNativeWindow(string) { select {} }

func (noEngine) newTab(int, string)   {}
func (noEngine) navigate(int, string) {}
func (noEngine) eval(context.Context, int, string, string) (string, error) {
	return "", errNoBrowser
}
func (noEngine) setTitle(int, string) {}
func (noEngine) selectTab(int)        {}
func (noEngine) closeTab(int)         {}
func (noEngine) activate()            {}
func (noEngine) setBusy(bool)         {}
