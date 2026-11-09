package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Плейсхолдеры и разметка, которые обязаны пережить перевод без изменений.
var placeholderRe = regexp.MustCompile(
	`\{\{[^{}]+\}\}` + // {{var}}
		`|\{[A-Za-z0-9_.:\-]+\}` + // {name}, {0}
		`|%(?:\d+\$)?[sdif@]` + // %s, %1$s, %d
		`|\[%[^%\]]+%\]` + // [%key%]
		`|</?[A-Za-z][^<>]*>`) // <b>, </i>, <br/>

var paragraphRe = regexp.MustCompile(`\n[ \t]*\n`)

// checkTranslation — дешёвая проверка до вставки в Lokalise. Ловит то, что потом
// пришлось бы искать руками: потерянные плейсхолдеры, склеенные абзацы,
// обрезанный или раздутый текст, «перевод» без перевода.
func checkTranslation(src, tr string) error {
	if strings.TrimSpace(tr) == "" {
		return errors.New("пустой перевод")
	}
	if a, b := placeholders(src), placeholders(tr); !slices.Equal(a, b) {
		return fmt.Errorf("плейсхолдеры/теги не совпадают: %v → %v", a, b)
	}
	if a, b := paragraphs(src), paragraphs(tr); a != b {
		return fmt.Errorf("абзацев %d, в переводе %d", a, b)
	}
	srcLen, trLen := utf8.RuneCountInString(strings.TrimSpace(src)), utf8.RuneCountInString(strings.TrimSpace(tr))
	if srcLen >= 20 {
		ratio := float64(trLen) / float64(srcLen)
		if ratio < 0.3 || ratio > 3.5 {
			return fmt.Errorf("подозрительная длина перевода: %d → %d символов", srcLen, trLen)
		}
	}
	if srcLen >= 40 && normalizeText(src) == normalizeText(tr) {
		return errors.New("текст вернулся без перевода")
	}
	return nil
}

func placeholders(s string) []string {
	found := placeholderRe.FindAllString(s, -1)
	slices.Sort(found)
	return found
}

func paragraphs(s string) int {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if s == "" {
		return 0
	}
	return len(paragraphRe.Split(s, -1))
}
