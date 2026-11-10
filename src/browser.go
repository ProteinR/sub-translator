package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

// Браузер — вкладки встроенного окна (WebKit). Вся работа со страницей Lokalise
// идёт через функции из lokalise.js, которые вызываются внутри вкладки.

//go:embed lokalise.js
var lokaliseJS string

var errNoBrowser = errors.New("встроенный браузер недоступен (нужен macOS)")

// tabEngine — низкоуровневые операции над вкладкой окна.
type tabEngine interface {
	newTab(id int, title string)
	navigate(id int, url string)
	eval(ctx context.Context, id int, body, argsJSON string) (string, error)
	setTitle(id int, title string)
	selectTab(id int)
	closeTab(id int)
	activate()
	setBusy(busy bool)
}

// browserTab — вкладка Lokalise, в которой работает один поток перевода.
type browserTab struct {
	id  int
	eng tabEngine
}

func (t *browserTab) Navigate(url string)   { t.eng.navigate(t.id, url) }
func (t *browserTab) SetTitle(title string) { t.eng.setTitle(t.id, title) }
func (t *browserTab) Show()                 { t.eng.selectTab(t.id); t.eng.activate() }
func (t *browserTab) Close()                { t.eng.closeTab(t.id) }

// Call вызывает window.__loka[op](args) и раскладывает JSON-ответ в out.
func (t *browserTab) Call(ctx context.Context, op string, args any, out any) error {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return err
	}
	opJSON, _ := json.Marshal(op)
	body := lokaliseJS + "\nreturn JSON.stringify(await window.__loka[" + string(opJSON) + "](JSON.parse(argsJSON)));"
	res, err := t.eng.eval(ctx, t.id, body, string(argsJSON))
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(res), out); err != nil {
		return fmt.Errorf("%s: ответ страницы не разобран: %w", op, err)
	}
	return nil
}

// Eval выполняет произвольный JS (отладка через /api/debug/eval).
func (t *browserTab) Eval(ctx context.Context, js string) (string, error) {
	return t.eng.eval(ctx, t.id, js, "null")
}
