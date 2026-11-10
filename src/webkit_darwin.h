// Нативное окно с вкладками WKWebView. Все функции можно звать из любого потока:
// работа уходит на главный поток. Результаты возвращаются в Go через lokaEvalDone.
#pragma once

void loka_run(const char *panelURL);
void loka_tab_new(int tab, const char *title);
void loka_tab_navigate(int tab, const char *url);
void loka_tab_eval(int tab, const char *body, const char *args, long long req);
void loka_tab_title(int tab, const char *title);
void loka_tab_select(int tab);
void loka_tab_close(int tab);
void loka_activate(void);
void loka_set_busy(int busy);
void loka_quit(void);
