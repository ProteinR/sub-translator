// Одно окно: вкладка «Панель» с веб-интерфейсом и по вкладке на каждый поток
// перевода. Вместо Playwright + Chromium — системный WebKit.
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include "webkit_darwin.h"
#include "_cgo_export.h"

// Похоже на Safari: часть сайтов отказывается работать с «неизвестным» WebView.
static NSString *const kUserAgentSuffix = @"Version/26.0 Safari/605.1.15";

@interface LokaTab : NSObject
@property(nonatomic) int tabID;
@property(nonatomic, strong) WKWebView *web;
@property(nonatomic, copy) NSString *title;
@end
@implementation LokaTab
@end

@interface LokaFlippedView : NSView
@end
@implementation LokaFlippedView
- (BOOL)isFlipped { return YES; }
@end

@interface LokaController : NSObject <NSApplicationDelegate, NSWindowDelegate, WKUIDelegate, WKNavigationDelegate>
@property(nonatomic, strong) NSWindow *window;
@property(nonatomic, strong) NSSegmentedControl *tabBar;
@property(nonatomic, strong) NSView *content;
@property(nonatomic, strong) NSMutableArray<LokaTab *> *tabs;
@property(nonatomic) int selectedID;
@property(nonatomic, copy) NSString *panelURL;
@property(nonatomic, strong) id activity;
@end

static LokaController *gController;

static NSString *str(const char *s) { return s ? [NSString stringWithUTF8String:s] : @""; }

static NSInteger compareTabs(id a, id b, void *ctx) {
    // Выбранная вкладка — последней, то есть поверх остальных.
    WKWebView *sel = (__bridge WKWebView *)ctx;
    if (a == sel) return NSOrderedDescending;
    if (b == sel) return NSOrderedAscending;
    return NSOrderedSame;
}

@implementation LokaController

- (void)applicationDidFinishLaunching:(NSNotification *)n {
    self.tabs = [NSMutableArray array];
    [self buildMenu];

    NSRect frame = NSMakeRect(0, 0, 1280, 860);
    NSUInteger style = NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable |
                       NSWindowStyleMaskResizable;
    self.window = [[NSWindow alloc] initWithContentRect:frame styleMask:style backing:NSBackingStoreBuffered defer:NO];
    self.window.title = @"Loka Translator";
    self.window.delegate = self;
    self.window.releasedWhenClosed = NO;
    [self.window setFrameAutosaveName:@"LokaMainWindow"];
    if (![self.window setFrameUsingName:@"LokaMainWindow"]) [self.window center];

    NSView *root = self.window.contentView;
    const CGFloat barH = 34;
    LokaFlippedView *bar = [[LokaFlippedView alloc] initWithFrame:NSMakeRect(0, 0, root.bounds.size.width, barH)];
    bar.autoresizingMask = NSViewWidthSizable | NSViewMinYMargin;
    [bar setFrameOrigin:NSMakePoint(0, root.bounds.size.height - barH)];
    self.tabBar = [NSSegmentedControl segmentedControlWithLabels:@[] trackingMode:NSSegmentSwitchTrackingSelectOne
                                                          target:self action:@selector(tabClicked:)];
    self.tabBar.segmentStyle = NSSegmentStyleAutomatic;
    self.tabBar.frame = NSMakeRect(10, 5, 300, 24);
    [bar addSubview:self.tabBar];
    [root addSubview:bar];

    self.content = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, root.bounds.size.width, root.bounds.size.height - barH)];
    self.content.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    [root addSubview:self.content];

    [self addTab:0 title:@"Панель"];
    [[self tab:0].web loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:self.panelURL]]];
    [self select:0];

    [self.window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)app { return YES; }

- (void)buildMenu {
    NSMenu *bar = [[NSMenu alloc] init];
    NSMenuItem *appItem = [[NSMenuItem alloc] init];
    NSMenu *appMenu = [[NSMenu alloc] init];
    [appMenu addItemWithTitle:@"Скрыть Loka Translator" action:@selector(hide:) keyEquivalent:@"h"];
    [appMenu addItem:[NSMenuItem separatorItem]];
    [appMenu addItemWithTitle:@"Выйти из Loka Translator" action:@selector(terminate:) keyEquivalent:@"q"];
    appItem.submenu = appMenu;
    [bar addItem:appItem];

    NSMenuItem *editItem = [[NSMenuItem alloc] init];
    NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Правка"];
    [edit addItemWithTitle:@"Отменить" action:@selector(undo:) keyEquivalent:@"z"];
    NSMenuItem *redo = [edit addItemWithTitle:@"Повторить" action:@selector(redo:) keyEquivalent:@"z"];
    redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
    [edit addItem:[NSMenuItem separatorItem]];
    [edit addItemWithTitle:@"Вырезать" action:@selector(cut:) keyEquivalent:@"x"];
    [edit addItemWithTitle:@"Скопировать" action:@selector(copy:) keyEquivalent:@"c"];
    [edit addItemWithTitle:@"Вставить" action:@selector(paste:) keyEquivalent:@"v"];
    [edit addItemWithTitle:@"Выбрать все" action:@selector(selectAll:) keyEquivalent:@"a"];
    editItem.submenu = edit;
    [bar addItem:editItem];

    NSMenuItem *viewItem = [[NSMenuItem alloc] init];
    NSMenu *view = [[NSMenu alloc] initWithTitle:@"Вид"];
    [view addItemWithTitle:@"Обновить вкладку" action:@selector(reloadTab:) keyEquivalent:@"r"];
    [view addItemWithTitle:@"Назад" action:@selector(goBackTab:) keyEquivalent:@"["];
    [view addItemWithTitle:@"Вперёд" action:@selector(goForwardTab:) keyEquivalent:@"]"];
    [view addItem:[NSMenuItem separatorItem]];
    [view addItemWithTitle:@"Панель" action:@selector(showPanel:) keyEquivalent:@"1"];
    viewItem.submenu = view;
    [bar addItem:viewItem];

    NSApp.mainMenu = bar;
}

- (void)reloadTab:(id)sender { [[self tab:self.selectedID].web reload]; }
- (void)goBackTab:(id)sender { [[self tab:self.selectedID].web goBack]; }
- (void)goForwardTab:(id)sender { [[self tab:self.selectedID].web goForward]; }
- (void)showPanel:(id)sender { [self select:0]; }

- (LokaTab *)tab:(int)tabID {
    for (LokaTab *t in self.tabs)
        if (t.tabID == tabID) return t;
    return nil;
}

- (void)addTab:(int)tabID title:(NSString *)title {
    if ([self tab:tabID]) return;
    WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
    cfg.websiteDataStore = [WKWebsiteDataStore defaultDataStore]; // вход в Lokalise общий и сохраняется
    cfg.applicationNameForUserAgent = kUserAgentSuffix;
    @try {
        [cfg.preferences setValue:@YES forKey:@"developerExtrasEnabled"]; // правый клик → «Проверить элемент»
    } @catch (NSException *e) {
    }

    WKWebView *web = [[WKWebView alloc] initWithFrame:self.content.bounds configuration:cfg];
    web.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    web.UIDelegate = self;
    web.navigationDelegate = self;
    if (@available(macOS 13.3, *)) web.inspectable = YES;
    // Вкладки лежат стопкой в одном окне, поэтому WebKit считает их видимыми и не
    // замедляет таймеры фоновых потоков. Отключаем и проверку перекрытия окна,
    // чтобы перевод не тормозил, когда окно свёрнуто или закрыто другими окнами.
    SEL occl = NSSelectorFromString(@"_setWindowOcclusionDetectionEnabled:");
    if ([web respondsToSelector:occl]) {
        NSMethodSignature *sig = [web methodSignatureForSelector:occl];
        NSInvocation *inv = [NSInvocation invocationWithMethodSignature:sig];
        BOOL no = NO;
        inv.selector = occl;
        [inv setArgument:&no atIndex:2];
        [inv invokeWithTarget:web];
    }
    [self.content addSubview:web positioned:NSWindowBelow relativeTo:nil];

    LokaTab *t = [[LokaTab alloc] init];
    t.tabID = tabID;
    t.web = web;
    t.title = title;
    [self.tabs addObject:t];
    [self refreshBar];
}

- (void)refreshBar {
    self.tabBar.segmentCount = self.tabs.count;
    for (NSUInteger i = 0; i < self.tabs.count; i++) {
        NSString *title = self.tabs[i].title;
        if (title.length > 40) title = [[title substringToIndex:39] stringByAppendingString:@"…"];
        [self.tabBar setLabel:title forSegment:i];
        [self.tabBar setWidth:0 forSegment:i];
        if (self.tabs[i].tabID == self.selectedID) self.tabBar.selectedSegment = i;
    }
    [self.tabBar sizeToFit];
}

- (void)tabClicked:(NSSegmentedControl *)sender {
    NSInteger i = sender.selectedSegment;
    if (i >= 0 && i < (NSInteger)self.tabs.count) [self select:self.tabs[i].tabID];
}

- (void)select:(int)tabID {
    LokaTab *t = [self tab:tabID];
    if (!t) return;
    self.selectedID = tabID;
    // Не убираем остальные вкладки из окна, а только меняем порядок слоёв.
    [self.content sortSubviewsUsingFunction:compareTabs context:(__bridge void *)t.web];
    [self.window makeFirstResponder:t.web];
    [self refreshBar];
}

- (void)closeTab:(int)tabID {
    LokaTab *t = [self tab:tabID];
    if (!t || tabID == 0) return;
    [t.web stopLoading];
    [t.web removeFromSuperview];
    [self.tabs removeObject:t];
    if (self.selectedID == tabID) [self select:0];
    [self refreshBar];
}

- (void)evalIn:(int)tabID body:(NSString *)body args:(NSString *)args req:(long long)req {
    LokaTab *t = [self tab:tabID];
    if (!t) {
        lokaEvalDone(req, NULL, (char *)"вкладка закрыта");
        return;
    }
    [t.web callAsyncJavaScript:body
                     arguments:@{@"argsJSON" : args}
                       inFrame:nil
                inContentWorld:WKContentWorld.pageWorld
             completionHandler:^(id result, NSError *error) {
               if (error) {
                   NSString *msg = error.userInfo[@"WKJavaScriptExceptionMessage"] ?: error.localizedDescription;
                   lokaEvalDone(req, NULL, (char *)msg.UTF8String);
                   return;
               }
               NSString *out = [result isKindOfClass:NSString.class] ? result : @"null";
               lokaEvalDone(req, (char *)out.UTF8String, NULL);
             }];
}

// Ссылки с target=_blank открываем в обычном браузере.
- (WKWebView *)webView:(WKWebView *)webView
    createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration
               forNavigationAction:(WKNavigationAction *)action
                    windowFeatures:(WKWindowFeatures *)features {
    if (action.request.URL) [[NSWorkspace sharedWorkspace] openURL:action.request.URL];
    return nil;
}

- (void)webView:(WKWebView *)webView
    runJavaScriptAlertPanelWithMessage:(NSString *)message
                      initiatedByFrame:(WKFrameInfo *)frame
                     completionHandler:(void (^)(void))done {
    NSAlert *a = [[NSAlert alloc] init];
    a.messageText = message;
    [a beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse r) { done(); }];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptConfirmPanelWithMessage:(NSString *)message
                        initiatedByFrame:(WKFrameInfo *)frame
                       completionHandler:(void (^)(BOOL))done {
    NSAlert *a = [[NSAlert alloc] init];
    a.messageText = message;
    [a addButtonWithTitle:@"OK"];
    [a addButtonWithTitle:@"Отмена"];
    [a beginSheetModalForWindow:self.window
              completionHandler:^(NSModalResponse r) { done(r == NSAlertFirstButtonReturn); }];
}

- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation {
    if (webView == [self tab:0].web) return;
    for (LokaTab *t in self.tabs)
        if (t.web == webView && t.tabID == self.selectedID) self.window.subtitle = webView.title ?: @"";
}

@end

void loka_run(const char *panelURL) {
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        app.activationPolicy = NSApplicationActivationPolicyRegular;
        gController = [[LokaController alloc] init];
        gController.panelURL = str(panelURL);
        app.delegate = gController;
        [app run];
    }
}

void loka_tab_new(int tab, const char *title) {
    NSString *t = str(title);
    dispatch_async(dispatch_get_main_queue(), ^{ [gController addTab:tab title:t]; });
}

void loka_tab_navigate(int tab, const char *url) {
    NSURL *u = [NSURL URLWithString:str(url)];
    dispatch_async(dispatch_get_main_queue(), ^{
      if (u) [[gController tab:tab].web loadRequest:[NSURLRequest requestWithURL:u]];
    });
}

void loka_tab_eval(int tab, const char *body, const char *args, long long req) {
    NSString *b = str(body), *a = str(args);
    dispatch_async(dispatch_get_main_queue(), ^{ [gController evalIn:tab body:b args:a req:req]; });
}

void loka_tab_title(int tab, const char *title) {
    NSString *t = str(title);
    dispatch_async(dispatch_get_main_queue(), ^{
      LokaTab *x = [gController tab:tab];
      if (!x) return;
      x.title = t;
      [gController refreshBar];
    });
}

void loka_tab_select(int tab) {
    dispatch_async(dispatch_get_main_queue(), ^{ [gController select:tab]; });
}

void loka_tab_close(int tab) {
    dispatch_async(dispatch_get_main_queue(), ^{ [gController closeTab:tab]; });
}

void loka_activate(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
      [gController.window makeKeyAndOrderFront:nil];
      [NSApp activateIgnoringOtherApps:YES];
    });
}

// Во время перевода не даём macOS усыплять приложение (App Nap) и систему.
void loka_set_busy(int busy) {
    dispatch_async(dispatch_get_main_queue(), ^{
      NSProcessInfo *pi = [NSProcessInfo processInfo];
      if (busy && !gController.activity) {
          gController.activity = [pi beginActivityWithOptions:NSActivityUserInitiated reason:@"Перевод в Lokalise"];
      } else if (!busy && gController.activity) {
          [pi endActivity:gController.activity];
          gController.activity = nil;
      }
    });
}
