// Twincxt for Linux: one GTK window with client-side decorations. The shell
// (ui/shell.html: sidebar, title strip = header bar, settings) fills it; the
// WhatsApp and Telegram views lie over its content area in a GtkOverlay.
// Behaviour follows desktop/main.js (the Electron version this replaces).
#define _GNU_SOURCE
#include "twincxt.h"

#include <gio/gio.h>
#include <gio/gdesktopappinfo.h>
#include <glib/gstdio.h>
#include <dlfcn.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#ifdef GDK_WINDOWING_X11
#include <gdk/gdkx.h>
#endif

#define SIDE_W 76
#define STRIP_H 46
#define MIN_W 960
#define MIN_H 620
#define NBLUR 5

static const char *BLUR_KEYS[NBLUR] = {"blurMessages", "blurPreviews", "blurNames", "blurPhotos", "blurMedia"};

// ---- config -------------------------------------------------------------------

typedef struct {
  char *active, *theme, *closeAction, *notifyMode, *telegramVersion, *skippedVersion;
  int zoomWa, zoomTg;
  gboolean autostart, startHidden, notifyWa, notifyTg, autoUpdate;
  gboolean blur[NBLUR];
  int wx, wy, ww, wh;
  gboolean wmax;
  // App lock: PBKDF2 hash and salt of the PIN (applock.c), hex.
  gboolean lockEnabled;
  char *lockHash, *lockSalt;
  int lockAfter, lockFails;  // minutes in the background (0 = only on start); wrong tries in a row
  gint64 lockUntil;          // unix seconds; no tries before this
} Config;

static Config cfg;
static char *config_dir, *config_path, *data_dir, *cache_dir, *log_path, *updates_dir, *ui_dir;
static gboolean debug_on;

void tw_log(const char *fmt, ...) {
  if (!debug_on) return;
  va_list ap;
  va_start(ap, fmt);
  char *msg = g_strdup_vprintf(fmt, ap);
  va_end(ap);
  GDateTime *now = g_date_time_new_now_local();
  char *ts = g_date_time_format(now, "%H:%M:%S");
  FILE *f = log_path ? fopen(log_path, "a") : NULL;
  if (f) {
    fprintf(f, "%s.%03d  %s\n", ts, g_date_time_get_microsecond(now) / 1000, msg);
    fclose(f);
  }
  if (g_getenv("TWINCXT_SMOKE")) fprintf(stderr, "[twincxt] %s\n", msg);
  g_free(ts);
  g_date_time_unref(now);
  g_free(msg);
}

static void str_set(char **dst, const char *v) {
  char *n = g_strdup(v ? v : "");
  g_free(*dst);
  *dst = n;
}

static gboolean one_of(const char *v, const char *a, const char *b, const char *c) {
  return v && ((a && !strcmp(v, a)) || (b && !strcmp(v, b)) || (c && !strcmp(v, c)));
}

static void cfg_defaults(void) {
  str_set(&cfg.active, "wa");
  str_set(&cfg.theme, "system");
  str_set(&cfg.closeAction, "tray");
  str_set(&cfg.notifyMode, "toast");
  str_set(&cfg.telegramVersion, "a");
  str_set(&cfg.skippedVersion, "");
  cfg.zoomWa = cfg.zoomTg = 100;
  cfg.autostart = cfg.startHidden = FALSE;
  cfg.notifyWa = cfg.notifyTg = cfg.autoUpdate = TRUE;
  for (int i = 0; i < NBLUR; i++) cfg.blur[i] = FALSE;
  cfg.wx = cfg.wy = 0;
  cfg.ww = 1240;
  cfg.wh = 800;
  cfg.wmax = FALSE;
  cfg.lockEnabled = FALSE;
  str_set(&cfg.lockHash, "");
  str_set(&cfg.lockSalt, "");
  cfg.lockAfter = 5;
  cfg.lockFails = 0;
  cfg.lockUntil = 0;
}

static void load_config(void) {
  cfg_defaults();
  char *txt = NULL;
  if (!g_file_get_contents(config_path, &txt, NULL, NULL)) return;
  const char *p = txt;
  if ((unsigned char)p[0] == 0xEF && (unsigned char)p[1] == 0xBB && (unsigned char)p[2] == 0xBF) p += 3;
  JSCValue *o = json_parse(p);
  g_free(txt);
  if (!o) return;
  char *s;
#define CFG_STR(key, field, a, b, c) \
  if ((s = jstr(o, key))) { if (one_of(s, a, b, c)) str_set(&cfg.field, s); g_free(s); }
  CFG_STR("active", active, "wa", "tg", NULL);
  CFG_STR("theme", theme, "light", "dark", "system");
  CFG_STR("closeAction", closeAction, "tray", "quit", NULL);
  CFG_STR("notifyMode", notifyMode, "toast", "attention", "both");
  CFG_STR("telegramVersion", telegramVersion, "a", "k", NULL);
#undef CFG_STR
  if ((s = jstr(o, "skippedVersion"))) { str_set(&cfg.skippedVersion, s); g_free(s); }
  double z = jnum(o, "zoomWa", 100);
  cfg.zoomWa = (z >= 50 && z <= 200) ? (int)z : 100;
  z = jnum(o, "zoomTg", 100);
  cfg.zoomTg = (z >= 50 && z <= 200) ? (int)z : 100;
  cfg.autostart = jbool(o, "autostart", cfg.autostart);
  cfg.startHidden = jbool(o, "startHidden", cfg.startHidden);
  cfg.notifyWa = jbool(o, "notifyWa", cfg.notifyWa);
  cfg.notifyTg = jbool(o, "notifyTg", cfg.notifyTg);
  cfg.autoUpdate = jbool(o, "autoUpdate", cfg.autoUpdate);
  for (int i = 0; i < NBLUR; i++) cfg.blur[i] = jbool(o, BLUR_KEYS[i], FALSE);
  cfg.lockEnabled = jbool(o, "lockEnabled", FALSE);
  if ((s = jstr(o, "lockHash"))) { str_set(&cfg.lockHash, s); g_free(s); }
  if ((s = jstr(o, "lockSalt"))) { str_set(&cfg.lockSalt, s); g_free(s); }
  int la = (int)jnum(o, "lockAfter", 5);
  cfg.lockAfter = (la == 0 || la == 1 || la == 5 || la == 15) ? la : 5;
  cfg.lockFails = (int)jnum(o, "lockFails", 0);
  cfg.lockUntil = (gint64)jnum(o, "lockUntil", 0);
  if (cfg.lockEnabled && !*cfg.lockHash) cfg.lockEnabled = FALSE;
  JSCValue *w = jobj(o, "window");
  if (w) {
    cfg.wx = (int)jnum(w, "x", 0);
    cfg.wy = (int)jnum(w, "y", 0);
    cfg.ww = (int)jnum(w, "w", 1240);
    cfg.wh = (int)jnum(w, "h", 800);
    cfg.wmax = jbool(w, "maximized", FALSE);
    g_object_unref(w);
  }
  g_object_unref(o);
}

static void kv_str(GString *g, const char *k, const char *v) {
  g_string_append_printf(g, ",\n  \"%s\": ", k);
  json_esc(g, v);
}
static void kv_bool(GString *g, const char *k, gboolean v) { g_string_append_printf(g, ",\n  \"%s\": %s", k, v ? "true" : "false"); }
static void kv_int(GString *g, const char *k, int v) { g_string_append_printf(g, ",\n  \"%s\": %d", k, v); }

static void save_config(void) {
  GString *g = g_string_new("{\n  \"active\": ");
  json_esc(g, cfg.active);
  kv_str(g, "theme", cfg.theme);
  kv_int(g, "zoomWa", cfg.zoomWa);
  kv_int(g, "zoomTg", cfg.zoomTg);
  kv_bool(g, "autostart", cfg.autostart);
  kv_bool(g, "startHidden", cfg.startHidden);
  kv_str(g, "closeAction", cfg.closeAction);
  kv_bool(g, "notifyWa", cfg.notifyWa);
  kv_bool(g, "notifyTg", cfg.notifyTg);
  kv_str(g, "notifyMode", cfg.notifyMode);
  kv_str(g, "telegramVersion", cfg.telegramVersion);
  kv_bool(g, "autoUpdate", cfg.autoUpdate);
  kv_str(g, "skippedVersion", cfg.skippedVersion);
  for (int i = 0; i < NBLUR; i++) kv_bool(g, BLUR_KEYS[i], cfg.blur[i]);
  kv_bool(g, "lockEnabled", cfg.lockEnabled);
  kv_str(g, "lockHash", cfg.lockHash);
  kv_str(g, "lockSalt", cfg.lockSalt);
  kv_int(g, "lockAfter", cfg.lockAfter);
  kv_int(g, "lockFails", cfg.lockFails);
  g_string_append_printf(g, ",\n  \"lockUntil\": %" G_GINT64_FORMAT, cfg.lockUntil);
  g_string_append_printf(g, ",\n  \"window\": {\n    \"x\": %d,\n    \"y\": %d,\n    \"w\": %d,\n    \"h\": %d,\n    \"maximized\": %s\n  }\n}\n",
                         cfg.wx, cfg.wy, cfg.ww, cfg.wh, cfg.wmax ? "true" : "false");
  GError *err = NULL;
  g_mkdir_with_parents(config_dir, 0700);
  if (!g_file_set_contents(config_path, g->str, (gssize)g->len, &err)) {
    tw_log("config: %s", err->message);
    g_error_free(err);
  }
  g_string_free(g, TRUE);
}

gboolean cfg_auto_update(void) { return cfg.autoUpdate; }
const char *cfg_skipped_version(void) { return cfg.skippedVersion; }
void cfg_set_skipped_version(const char *v) { str_set(&cfg.skippedVersion, v); save_config(); }
const char *app_updates_dir(void) { return updates_dir; }

// ---- state ----------------------------------------------------------------------

typedef struct Svc Svc;
typedef struct Popup Popup;
typedef struct {
  Svc *svc;
  gboolean popup;
  Popup *p;
} ViewInfo;

struct Svc {
  const char *id, *name;
  WebKitWebView *view;
  ViewInfo info;
  int unread;
  gboolean has_strip;
  int r, g, b;
  gboolean ready;
};

struct Popup {
  ViewInfo info;
  WebKitWebView *view;
  GtkWidget *win;
  gboolean closing, loaded, dead;
  guint timer;
};

static Svc svcs[2] = {{.id = "wa", .name = "WhatsApp"}, {.id = "tg", .name = "Telegram"}};
static GList *popups;

static GtkApplication *app;
static GtkWidget *win, *overlay;
static WebKitWebView *shell;
static WebKitWebContext *wctx;
static WebKitSettings *svc_settings;
static gboolean app_locked;  // the app lock screen is showing
static gint64 away_since;    // monotonic time Twincxt went to the background, 0: in front
static gboolean shell_ready, settings_open, locked, quitting, maximized, fullscreen, iconified, started;
static Svc *full_svc;
static GdkEvent *last_press;
static char *decoration_layout;
static char *service_js_src;
static char *user_agent;
static gboolean system_dark;
static int normal_w, normal_h;
static guint notify_counter;
static GSimpleAction *open_link_action;

static Svc *svc_by_id(const char *id) {
  if (!id) return NULL;
  for (int i = 0; i < 2; i++)
    if (!strcmp(svcs[i].id, id)) return &svcs[i];
  return NULL;
}
static Svc *active_svc(void) { return svc_by_id(cfg.active) ? svc_by_id(cfg.active) : &svcs[0]; }

static char *service_url(const char *id) {
  if (!strcmp(id, "wa")) return g_strdup("https://web.whatsapp.com/");
  return g_strdup_printf("https://web.telegram.org/%s/", cfg.telegramVersion);
}

static gboolean is_dark(void) {
  return !strcmp(cfg.theme, "dark") || (!strcmp(cfg.theme, "system") && system_dark);
}

static gboolean host_in(const char *host, const char *const *domains) {
  if (!host) return FALSE;
  for (int i = 0; domains[i]; i++) {
    size_t hl = strlen(host), dl = strlen(domains[i]);
    if (!g_ascii_strcasecmp(host, domains[i])) return TRUE;
    if (hl > dl && host[hl - dl - 1] == '.' && !g_ascii_strcasecmp(host + hl - dl, domains[i])) return TRUE;
  }
  return FALSE;
}

static gboolean trusted(const char *uri) {
  static const char *const DOMAINS[] = {"whatsapp.com", "whatsapp.net", "telegram.org", NULL};
  if (!uri || g_ascii_strncasecmp(uri, "https:", 6)) return FALSE;
  GUri *u = g_uri_parse(uri, G_URI_FLAGS_NONE, NULL);
  if (!u) return FALSE;
  gboolean ok = host_in(g_uri_get_host(u), DOMAINS);
  g_uri_unref(u);
  return ok;
}

static gboolean popupable(const char *uri) {
  return !uri || !*uri || g_str_has_prefix(uri, "about:") || trusted(uri);
}

static gboolean is_web(const char *uri) {
  return uri && (!g_ascii_strncasecmp(uri, "http:", 5) || !g_ascii_strncasecmp(uri, "https:", 6));
}

static void open_external(const char *uri) {
  if (!uri || !(is_web(uri) || !g_ascii_strncasecmp(uri, "mailto:", 7) || !g_ascii_strncasecmp(uri, "tel:", 4))) return;
  tw_log("open in browser: %.200s", uri);
  GError *err = NULL;
  if (!gtk_show_uri_on_window(win ? GTK_WINDOW(win) : NULL, uri, GDK_CURRENT_TIME, &err)) {
    tw_log("open in browser failed: %s", err->message);
    g_error_free(err);
  }
}

// A Chrome user agent: WhatsApp refuses browsers it does not recognise. The
// version follows Chrome's 4-week release train (131 shipped on 2024-11-12).
static char *make_user_agent(void) {
  const char *e = g_getenv("TWINCXT_UA");
  if (e && *e) return g_strdup(e);
  gint64 now = g_get_real_time() / G_USEC_PER_SEC;
  int v = 131 + (int)((now - 1731369600) / (28 * 86400));
  if (v < 131) v = 131;
  return g_strdup_printf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", v);
}

// Clicks WhatsApp's own "Move here" so a call (and the camera) returns to the
// main window when you close the call window.
static const char *MOVE_HERE_JS =
    "(function(){"
    "var re=/move here|pindahkan ke sini|pindah ke sini|mover aqu|mover para c|hierher|ici/i;"
    "var els=document.querySelectorAll('button,[role=\"button\"]');"
    "for(var i=0;i<els.length;i++){"
    "var l=(els[i].getAttribute('aria-label')||'')+' '+(els[i].getAttribute('title')||'')+' '+(els[i].textContent||'');"
    "if(re.test(l)){els[i].click();return true;}"
    "}"
    "return false;"
    "})()";

// ---- shell ----------------------------------------------------------------------

static void run_js(WebKitWebView *v, const char *js) {
  if (v) webkit_web_view_run_javascript(v, js, NULL, NULL, NULL);
}

static void shell_eval(const char *js) {
  if (shell_ready && shell) run_js(shell, js);
}

static void push_state(void) {
  GString *g = g_string_new("twincxt.state({\"platform\":\"linux\",\"version\":");
  json_esc(g, TWINCXT_VERSION);
#define S(k, v) g_string_append(g, ",\"" k "\":"), json_esc(g, v)
#define B(k, v) g_string_append_printf(g, ",\"" k "\":%s", (v) ? "true" : "false")
  S("active", cfg.active);
  S("theme", cfg.theme);
  g_string_append_printf(g, ",\"zoom\":{\"wa\":%d,\"tg\":%d}", cfg.zoomWa, cfg.zoomTg);
  B("autostart", cfg.autostart);
  B("startHidden", cfg.startHidden);
  S("closeAction", cfg.closeAction);
  B("notifyWa", cfg.notifyWa);
  B("notifyTg", cfg.notifyTg);
  S("notifyMode", cfg.notifyMode);
  S("telegramVersion", cfg.telegramVersion);
  B("autoUpdate", cfg.autoUpdate);
  for (int i = 0; i < NBLUR; i++) g_string_append_printf(g, ",\"%s\":%s", BLUR_KEYS[i], cfg.blur[i] ? "true" : "false");
  B("lockEnabled", cfg.lockEnabled);
  B("locked", app_locked);
  g_string_append_printf(g, ",\"lockAfter\":%d", cfg.lockAfter);
  B("maximized", maximized);
  B("settingsOpen", settings_open);
  S("decorationLayout", decoration_layout ? decoration_layout : "menu:close");
#undef S
#undef B
  g_string_append(g, "})");
  shell_eval(g->str);
  g_string_free(g, TRUE);
}

static void update_launcher_count(int total) {
  // Dock badges (Ubuntu Dock, Dash to Dock, KDE, Plank) through the Unity launcher API.
  GDBusConnection *bus = g_application_get_dbus_connection(G_APPLICATION(app));
  if (!bus) return;
  GVariantBuilder b;
  g_variant_builder_init(&b, G_VARIANT_TYPE("a{sv}"));
  g_variant_builder_add(&b, "{sv}", "count", g_variant_new_int64(total));
  g_variant_builder_add(&b, "{sv}", "count-visible", g_variant_new_boolean(total > 0));
  g_dbus_connection_emit_signal(bus, NULL, "/io/github/rashiiddsr/Twincxt/launcher", "com.canonical.Unity.LauncherEntry", "Update",
                                g_variant_new("(sa{sv})", "application://" APP_ID ".desktop", &b), NULL);
}

static void push_unread(Svc *s) {
  char *js = g_strdup_printf("twincxt.unread(\"%s\",%d)", s->id, s->unread);
  shell_eval(js);
  g_free(js);
  int total = svcs[0].unread + svcs[1].unread;
  char *title = total ? g_strdup_printf("Twincxt (%d)", total) : g_strdup("Twincxt");
  if (win) gtk_window_set_title(GTK_WINDOW(win), title);
  g_free(title);
  tray_update(svcs[0].unread, svcs[1].unread);
  update_launcher_count(total);
}

static void push_loading(Svc *s, gboolean on) {
  char *js = g_strdup_printf("twincxt.loading(\"%s\",%s)", s->id, on ? "true" : "false");
  shell_eval(js);
  g_free(js);
}

static void apply_strip(void) {
  Svc *s = active_svc();
  if (settings_open || !s->has_strip) {
    shell_eval("twincxt.strip('', '')");
    return;
  }
  gboolean dark = (0.299 * s->r + 0.587 * s->g + 0.114 * s->b) / 255 < 0.5;
  char *js = g_strdup_printf("twincxt.strip(\"rgb(%d,%d,%d)\",\"%s\")", s->r, s->g, s->b, dark ? "#D1D7DB" : "#3B4A54");
  shell_eval(js);
  g_free(js);
}

static char *privacy_js(void) {
  GString *g = g_string_new("window.twincxtPrivacy && window.twincxtPrivacy({");
  for (int i = 0; i < NBLUR; i++) g_string_append_printf(g, "%s\"%s\":%s", i ? "," : "", BLUR_KEYS[i], cfg.blur[i] ? "true" : "false");
  g_string_append(g, "})");
  return g_string_free(g, FALSE);
}

static void apply_privacy(Svc *only) {
  char *js = privacy_js();
  for (int i = 0; i < 2; i++)
    if (!only || only == &svcs[i]) run_js(svcs[i].view, js);
  g_free(js);
}

// ---- window and layout -------------------------------------------------------------

static void layout(void) {
  for (int i = 0; i < 2; i++) {
    Svc *s = &svcs[i];
    if (!s->view) continue;
    GtkWidget *w = GTK_WIDGET(s->view);
    gboolean full = full_svc == s && !app_locked;
    gtk_widget_set_margin_start(w, full ? 0 : SIDE_W);
    gtk_widget_set_margin_top(w, full ? 0 : STRIP_H);
    gboolean visible = full || (!full_svc && !settings_open && !app_locked && s == active_svc());
    gtk_widget_set_visible(w, visible);
  }
}

static void focus_active(void) {
  if (settings_open || app_locked || !active_svc()->view) {
    if (shell) gtk_widget_grab_focus(GTK_WIDGET(shell));
    return;
  }
  gtk_widget_grab_focus(GTK_WIDGET(active_svc()->view));
}

static void select_service(const char *id, const char *source) {
  if (!svc_by_id(id)) return;
  if (locked) {
    settings_open = TRUE;
    layout();
    push_state();
    return;
  }
  tw_log("select %s by %s", id, source);
  str_set(&cfg.active, id);
  settings_open = FALSE;
  layout();
  push_state();
  apply_strip();
  focus_active();
  save_config();
}

static void toggle_settings(int on) {  // -1: toggle
  settings_open = locked || (on < 0 ? !settings_open : on);
  layout();
  push_state();
  apply_strip();
  focus_active();
}

void app_show_window(void) {
  if (!win) return;
  gtk_widget_show(win);
  gtk_window_deiconify(GTK_WINDOW(win));
  gtk_window_present(GTK_WINDOW(win));
  focus_active();
}

void app_select(const char *id) {
  select_service(id, "tray");
  app_show_window();
}

void app_open_settings(void) {
  toggle_settings(1);
  app_show_window();
}

void app_lock_for_update(void) {
  if (locked) return;
  locked = TRUE;
  tw_log("update: below the minimum version, services locked");
  settings_open = TRUE;
  layout();
  push_state();
  apply_strip();
  app_show_window();
}

void app_update_state(const char *json) {
  char *js = g_strdup_printf("twincxt.update(%s)", json);
  shell_eval(js);
  g_free(js);
}

static void save_bounds(void) {
  if (!win || !gtk_widget_get_realized(win) || iconified || fullscreen) return;
  if (!maximized && normal_w > 0 && normal_h > 0) {
    cfg.ww = normal_w;
    cfg.wh = normal_h;
  }
  cfg.wmax = maximized;
  save_config();
}

static void destroy_popups(void) {
  GList *copy = g_list_copy(popups);
  for (GList *l = copy; l; l = l->next) {
    Popup *p = l->data;
    if (p->win) gtk_widget_destroy(p->win);
  }
  g_list_free(copy);
}

void app_quit(void) {
  if (quitting && !win) return;
  quitting = TRUE;
  save_bounds();
  save_config();
  destroy_popups();
  if (win) {
    GtkWidget *w = win;
    win = NULL;
    gtk_widget_destroy(w);
  }
  g_application_quit(G_APPLICATION(app));
}

static void close_main(void) {
  save_bounds();
  if (!quitting && !strcmp(cfg.closeAction, "tray")) {
    if (tray_available()) gtk_widget_hide(win);
    else gtk_window_iconify(GTK_WINDOW(win));  // no tray to come back from
    return;
  }
  app_quit();
}

static gboolean on_delete(GtkWidget *w, GdkEvent *e, gpointer d) {
  close_main();
  return TRUE;
}

static gboolean on_window_state(GtkWidget *w, GdkEventWindowState *e, gpointer d) {
  gboolean was = maximized;
  maximized = (e->new_window_state & GDK_WINDOW_STATE_MAXIMIZED) != 0;
  fullscreen = (e->new_window_state & GDK_WINDOW_STATE_FULLSCREEN) != 0;
  iconified = (e->new_window_state & GDK_WINDOW_STATE_ICONIFIED) != 0;
  if (was != maximized) push_state();
  return FALSE;
}

static gboolean on_configure(GtkWidget *w, GdkEvent *e, gpointer d) {
  if (!maximized && !fullscreen && !iconified) {
    gint ww, hh;
    gtk_window_get_size(GTK_WINDOW(win), &ww, &hh);
    if (ww >= MIN_W / 2 && hh >= MIN_H / 2) {
      normal_w = ww;
      normal_h = hh;
    }
  }
  return FALSE;
}

static void check_away(void);
static gboolean on_focus_in(GtkWidget *w, GdkEvent *e, gpointer d) {
  gtk_window_set_urgency_hint(GTK_WINDOW(win), FALSE);
  check_away();  // locks before you see the chats if you were away long enough
  away_since = 0;
  return FALSE;
}

// The shell's title strip asks to move the window ("drag") right after the
// button press it saw. GTK needs that press (button, position, time) to hand the
// move to the window manager, on X11 and Wayland alike.
static gboolean on_shell_press(GtkWidget *w, GdkEventButton *e, gpointer d) {
  if (e->type == GDK_BUTTON_PRESS && e->button == GDK_BUTTON_PRIMARY) {
    if (last_press) gdk_event_free(last_press);
    last_press = gdk_event_copy((GdkEvent *)e);
  }
  return FALSE;
}

static void begin_drag(void) {
  if (!last_press || !win) return;
  GdkWindow *gw = gtk_widget_get_window(win);
  GdkSeat *seat = gdk_display_get_default_seat(gtk_widget_get_display(win));
  GdkDevice *ptr = seat ? gdk_seat_get_pointer(seat) : NULL;
  GdkModifierType mask = 0;
  if (gw && ptr) gdk_window_get_device_position(gw, ptr, NULL, NULL, &mask);
  if (!(mask & GDK_BUTTON1_MASK)) return;  // already released: a plain click
  GdkEventButton *b = &last_press->button;
  gtk_window_begin_move_drag(GTK_WINDOW(win), (gint)b->button, (gint)b->x_root, (gint)b->y_root, b->time);
  // The window manager now owns the pointer and the page never sees the
  // button go up; tell it, or it would keep selecting text on the next hover.
  GdkEvent *rel = gdk_event_copy(last_press);
  rel->type = GDK_BUTTON_RELEASE;
  gtk_widget_event(GTK_WIDGET(shell), rel);
  gdk_event_free(rel);
}

static void toggle_maximize(void) {
  if (maximized) gtk_window_unmaximize(GTK_WINDOW(win));
  else gtk_window_maximize(GTK_WINDOW(win));
}

// ---- zoom and options ------------------------------------------------------------------

static void set_zoom(const char *id, int z) {
  Svc *s = svc_by_id(id);
  if (!s) return;
  if (z < 50 || z > 200) z = 100;
  if (s == &svcs[0]) cfg.zoomWa = z;
  else cfg.zoomTg = z;
  if (s->view) webkit_web_view_set_zoom_level(s->view, z / 100.0);
  save_config();
}

static void zoom_by(int dir) {
  static const int steps[] = {50, 67, 75, 80, 90, 100, 110, 125, 150, 175, 200};
  const int n = G_N_ELEMENTS(steps);
  Svc *s = active_svc();
  int cur = s == &svcs[0] ? cfg.zoomWa : cfg.zoomTg, i = 5;
  for (int k = 0; k < n; k++)
    if (steps[k] == cur) i = k;
  int v = dir == 0 ? 100 : steps[CLAMP(i + dir, 0, n - 1)];
  set_zoom(s->id, v);
  push_state();
}

static void set_view_backgrounds(void) {
  GdkRGBA c;
  gdk_rgba_parse(&c, is_dark() ? "#1A1A1A" : "#EBEBEB");
  if (shell) webkit_web_view_set_background_color(shell, &c);
  gdk_rgba_parse(&c, is_dark() ? "#161717" : "#FFFFFF");
  for (int i = 0; i < 2; i++)
    if (svcs[i].view) webkit_web_view_set_background_color(svcs[i].view, &c);
}

static void apply_theme(void) {
  // WebKitGTK's prefers-color-scheme follows this setting: the shell, WhatsApp
  // and Telegram all switch with it.
  g_object_set(gtk_settings_get_default(), "gtk-application-prefer-dark-theme", is_dark(), NULL);
  set_view_backgrounds();
}

static char *self_exe(void) {
  const char *ai = g_getenv("APPIMAGE");
  if (ai && *ai) return g_strdup(ai);
  char *p = g_file_read_link("/proc/self/exe", NULL);
  return p ? p : g_strdup("twincxt");
}

static char *autostart_file(void) { return g_build_filename(g_get_user_config_dir(), "autostart", "twincxt.desktop", NULL); }

static void set_autostart(gboolean on) {
  char *f = autostart_file();
  if (on) {
    char *dir = g_path_get_dirname(f);
    g_mkdir_with_parents(dir, 0755);
    char *exe = self_exe();
    char *txt = g_strdup_printf("[Desktop Entry]\nType=Application\nName=Twincxt\nIcon=twincxt\nExec=\"%s\" --autostart\nX-GNOME-Autostart-enabled=true\n", exe);
    GError *err = NULL;
    if (!g_file_set_contents(f, txt, -1, &err)) {
      tw_log("autostart: %s", err->message);
      g_error_free(err);
    }
    g_free(txt);
    g_free(exe);
    g_free(dir);
  } else {
    g_unlink(f);
  }
  g_free(f);
}

static gboolean autostart_enabled(void) {
  char *f = autostart_file();
  gboolean r = g_file_test(f, G_FILE_TEST_EXISTS);
  g_free(f);
  return r;
}

static void reload_service(Svc *s) {
  char *u = service_url(s->id);
  webkit_web_view_load_uri(s->view, u);
  g_free(u);
}

static void set_option(const char *key, JSCValue *value) {
  gboolean b = value && jsc_value_to_boolean(value);
  char *str = value && jsc_value_is_string(value) ? jsc_value_to_string(value) : NULL;
  if (!strcmp(key, "theme")) {
    if (one_of(str, "light", "dark", "system")) str_set(&cfg.theme, str);
    apply_theme();
  } else if (!strcmp(key, "autostart")) {
    cfg.autostart = b;
    set_autostart(b);
  } else if (!strcmp(key, "startHidden")) cfg.startHidden = b;
  else if (!strcmp(key, "closeAction")) str_set(&cfg.closeAction, g_strcmp0(str, "quit") ? "tray" : "quit");
  else if (!strcmp(key, "notifyWa")) cfg.notifyWa = b;
  else if (!strcmp(key, "notifyTg")) cfg.notifyTg = b;
  else if (!strcmp(key, "notifyMode")) {
    if (one_of(str, "toast", "attention", "both")) str_set(&cfg.notifyMode, str);
  } else if (!strcmp(key, "autoUpdate")) {
    cfg.autoUpdate = b;
    if (b) updater_check(FALSE);
  } else if (!strcmp(key, "lockAfter")) {
    int n = -1;
    if (str && g_ascii_isdigit(str[0])) n = atoi(str);
    else if (value && jsc_value_is_number(value)) n = jsc_value_to_int32(value);
    if (n == 0 || n == 1 || n == 5 || n == 15) cfg.lockAfter = n;
  } else if (!strcmp(key, "telegramVersion")) {
    if (one_of(str, "a", "k", NULL) && strcmp(str, cfg.telegramVersion)) {
      str_set(&cfg.telegramVersion, str);
      reload_service(&svcs[1]);
    }
  } else {
    for (int i = 0; i < NBLUR; i++)
      if (!strcmp(key, BLUR_KEYS[i])) {
        cfg.blur[i] = b;
        apply_privacy(NULL);
      }
  }
  g_free(str);
  save_config();
  push_state();
}

typedef struct {
  Svc *svc;
} ResetCtx;

static void on_data_removed(GObject *src, GAsyncResult *res, gpointer data) {
  Svc *s = data;
  GError *err = NULL;
  if (!webkit_website_data_manager_remove_finish(WEBKIT_WEBSITE_DATA_MANAGER(src), res, &err)) {
    tw_log("%s: reset failed: %s", s->id, err->message);
    g_error_free(err);
  }
  s->unread = 0;
  push_unread(s);
  reload_service(s);
}

static void on_data_fetched(GObject *src, GAsyncResult *res, gpointer data) {
  Svc *s = data;
  WebKitWebsiteDataManager *m = WEBKIT_WEBSITE_DATA_MANAGER(src);
  GError *err = NULL;
  GList *all = webkit_website_data_manager_fetch_finish(m, res, &err);
  if (err) {
    tw_log("%s: reset failed: %s", s->id, err->message);
    g_error_free(err);
  }
  static const char *const WA[] = {"whatsapp.com", "whatsapp.net", NULL};
  static const char *const TG[] = {"telegram.org", NULL};
  GList *mine = NULL;
  for (GList *l = all; l; l = l->next)
    if (host_in(webkit_website_data_get_name(l->data), s == &svcs[0] ? WA : TG)) mine = g_list_prepend(mine, l->data);
  tw_log("%s: clearing %u site data entries", s->id, g_list_length(mine));
  if (mine) webkit_website_data_manager_remove(m, WEBKIT_WEBSITE_DATA_ALL, mine, NULL, on_data_removed, s);
  else {
    s->unread = 0;
    push_unread(s);
    reload_service(s);
  }
  g_list_free(mine);
  g_list_free_full(all, (GDestroyNotify)webkit_website_data_unref);
}

static void run_action(const char *action) {
  char **parts = g_strsplit(action, ":", 2);
  Svc *s = parts[0] && parts[1] ? svc_by_id(parts[1]) : NULL;
  if (s && s->view) {
    if (!strcmp(parts[0], "reload")) reload_service(s);
    else if (!strcmp(parts[0], "reset")) {
      WebKitWebsiteDataManager *m = webkit_web_context_get_website_data_manager(wctx);
      webkit_website_data_manager_fetch(m, WEBKIT_WEBSITE_DATA_ALL, NULL, on_data_fetched, s);
    }
  }
  g_strfreev(parts);
}

// ---- app lock (applock.go on Windows) ---------------------------------------------------------
//
// The PIN (4 to 8 digits) is never stored, only its PBKDF2 hash. Twincxt locks
// when it starts, on the lock button / Ctrl+L / the tray's Lock, and after it
// has been in the background for lockAfter minutes (0 = only on start). After 5
// wrong PINs in a row each try waits 30 s, doubling up to 5 minutes; the count
// survives restarts. "Forgot PIN?" signs out of both services and removes the
// PIN. This keeps people at your desk out of your chats; it does not encrypt
// the data on disk.

#define FREE_TRIES 5
#define FIRST_WAIT_S 30
#define MAX_WAIT_S 300

static void update_tray_lock(void) { tray_set_lock(cfg.lockEnabled && !app_locked); }

void app_lock_now(void) {
  if (!cfg.lockEnabled || app_locked) return;
  tw_log("lock: locked");
  app_locked = TRUE;
  layout();
  push_state();
  apply_strip();
  focus_active();
  update_tray_lock();
}

static void unlock_app(void) {
  app_locked = FALSE;
  away_since = 0;
  tw_log("lock: unlocked");
  layout();
  push_state();
  apply_strip();
  focus_active();
  update_tray_lock();
}

static void check_away(void) {
  if (!cfg.lockEnabled || app_locked || cfg.lockAfter <= 0 || !away_since) return;
  if (g_get_monotonic_time() - away_since >= (gint64)cfg.lockAfter * 60 * G_USEC_PER_SEC) app_lock_now();
}

static void mark_away(void) {
  if (!away_since) away_since = g_get_monotonic_time();
}

// Runs every 15 s, so the window is already locked when it comes back.
static gboolean on_lock_timer(gpointer d) {
  if (!win || !gtk_widget_get_visible(win) || iconified || !gtk_window_is_active(GTK_WINDOW(win))) {
    mark_away();
    check_away();
  }
  return G_SOURCE_CONTINUE;
}

static gboolean on_focus_out(GtkWidget *w, GdkEvent *e, gpointer d) {
  mark_away();
  return FALSE;
}

// Checks a PIN and applies the wrong-PIN pauses. NULL when right, else the
// message for the lock screen.
static char *check_pin(const char *pin) {
  gint64 now = g_get_real_time();
  if (cfg.lockUntil * G_USEC_PER_SEC > now) {
    char *w = lock_wait_text(cfg.lockUntil * G_USEC_PER_SEC - now);
    char *m = g_strdup_printf("Too many tries. Try again in %s.", w);
    g_free(w);
    return m;
  }
  char *got = pin_hash(pin ? pin : "", cfg.lockSalt);
  gboolean ok = equal_ct(cfg.lockHash, got);
  g_free(got);
  if (ok) {
    cfg.lockFails = 0;
    cfg.lockUntil = 0;
    save_config();
    return NULL;
  }
  cfg.lockFails++;
  char *msg;
  if (cfg.lockFails >= FREE_TRIES) {
    int k = cfg.lockFails - FREE_TRIES;
    gint64 wait = k >= 4 ? MAX_WAIT_S : (gint64)FIRST_WAIT_S << k;
    if (wait > MAX_WAIT_S || wait <= 0) wait = MAX_WAIT_S;
    cfg.lockUntil = now / G_USEC_PER_SEC + wait;
    char *w = lock_wait_text(wait * G_USEC_PER_SEC);
    msg = g_strdup_printf("Wrong PIN. Try again in %s.", w);
    g_free(w);
  } else if (FREE_TRIES - cfg.lockFails <= 2) {
    int left = FREE_TRIES - cfg.lockFails;
    msg = g_strdup_printf("Wrong PIN. %d %s left before a pause.", left, left == 1 ? "try" : "tries");
  } else {
    msg = g_strdup("Wrong PIN.");
  }
  save_config();
  return msg;
}

static gboolean set_pin(const char *pin) {
  char *salt = pin_new_salt();
  if (!salt) return FALSE;
  char *h = pin_hash(pin, salt);
  str_set(&cfg.lockSalt, salt);
  str_set(&cfg.lockHash, h);
  cfg.lockFails = 0;
  cfg.lockUntil = 0;
  g_free(salt);
  g_free(h);
  return TRUE;
}

static void clear_lock(void) {
  cfg.lockEnabled = FALSE;
  str_set(&cfg.lockHash, "");
  str_set(&cfg.lockSalt, "");
  cfg.lockFails = 0;
  cfg.lockUntil = 0;
  save_config();
}

static void lock_reply(const char *op, gboolean ok, const char *error) {
  tw_log("lock: %s %s%s%s", op, ok ? "ok" : "refused", error ? ": " : "", error ? error : "");
  GString *g = g_string_new("twincxt.lockResult({\"op\":");
  json_esc(g, op);
  g_string_append_printf(g, ",\"ok\":%s,\"error\":", ok ? "true" : "false");
  json_esc(g, error ? error : "");
  g_string_append(g, "})");
  shell_eval(g->str);
  g_string_free(g, TRUE);
}

// The shell's "lock" messages: {op: set|change|disable|unlock|now|reset, pin, old}.
static void on_lock_message(JSCValue *data) {
  char *op = jstr(data, "op"), *pin = jstr(data, "pin"), *old = jstr(data, "old");
  char *msg = NULL;
  if (!op) goto out;
  if (!strcmp(op, "set")) {
    if (cfg.lockEnabled) lock_reply(op, FALSE, "App lock is already on.");
    else if (!pin_format_ok(pin)) lock_reply(op, FALSE, "Use 4 to 8 digits.");
    else if (!set_pin(pin)) lock_reply(op, FALSE, "Can't set the PIN.");
    else {
      cfg.lockEnabled = TRUE;
      save_config();
      tw_log("lock: turned on");
      lock_reply(op, TRUE, NULL);
      push_state();
      update_tray_lock();
    }
  } else if (!strcmp(op, "change")) {
    if ((msg = check_pin(old))) lock_reply(op, FALSE, msg);
    else if (!pin_format_ok(pin)) lock_reply(op, FALSE, "Use 4 to 8 digits.");
    else if (!set_pin(pin)) lock_reply(op, FALSE, "Can't set the PIN.");
    else {
      save_config();
      lock_reply(op, TRUE, NULL);
    }
  } else if (!strcmp(op, "disable")) {
    if ((msg = check_pin(old))) lock_reply(op, FALSE, msg);
    else {
      clear_lock();
      tw_log("lock: turned off");
      lock_reply(op, TRUE, NULL);
      push_state();
      update_tray_lock();
    }
  } else if (!strcmp(op, "unlock")) {
    if (!app_locked) goto out;
    if ((msg = check_pin(pin))) lock_reply(op, FALSE, msg);
    else {
      lock_reply(op, TRUE, NULL);
      unlock_app();
    }
  } else if (!strcmp(op, "now")) {
    app_lock_now();
  } else if (!strcmp(op, "reset")) {
    if (!app_locked) goto out;
    tw_log("lock: forgot PIN, signing out of both services");
    run_action("reset:wa");
    run_action("reset:tg");
    clear_lock();
    lock_reply(op, TRUE, NULL);
    unlock_app();
  }
out:
  g_free(msg);
  g_free(op);
  g_free(pin);
  g_free(old);
}

// ---- messages from the shell ---------------------------------------------------------------

static void on_shell_msg(WebKitUserContentManager *u, WebKitJavascriptResult *r, gpointer d) {
  JSCValue *v = webkit_javascript_result_get_js_value(r);
  if (!jsc_value_is_string(v)) return;
  char *raw = jsc_value_to_string(v);
  JSCValue *m = json_parse(raw);
  char *type = jstr(m, "type");
  if (!type) goto out;
  if (strcmp(type, "regions")) tw_log("shell: %.300s", raw);
  JSCValue *data = jprop(m, "data");
  char *ds = data && jsc_value_is_string(data) ? jsc_value_to_string(data) : NULL;

  if (!strcmp(type, "ready")) {
    shell_ready = TRUE;
    push_state();
    for (int i = 0; i < 2; i++) {
      push_unread(&svcs[i]);
      push_loading(&svcs[i], !svcs[i].ready);
    }
    apply_strip();
    char *u = updater_state_json();
    app_update_state(u);
    g_free(u);
  } else if (!strcmp(type, "select")) select_service(ds, "sidebar");
  else if (!strcmp(type, "settings")) toggle_settings(data && jsc_value_to_boolean(data));
  else if (!strcmp(type, "zoom")) {
    char *svc = jstr(data, "svc");
    set_zoom(svc, (int)jnum(data, "value", 100));
    g_free(svc);
  } else if (!strcmp(type, "set")) {
    char *key = jstr(data, "key");
    JSCValue *val = jprop(data, "value");
    if (key) set_option(key, val);
    g_clear_object(&val);
    g_free(key);
  } else if (!strcmp(type, "lock")) {
    on_lock_message(data);
  } else if (!strcmp(type, "action")) {
    if (ds) run_action(ds);
  } else if (!strcmp(type, "update")) {
    if (!g_strcmp0(ds, "check")) updater_check(TRUE);
    else if (!g_strcmp0(ds, "install")) updater_install();
    else if (!g_strcmp0(ds, "skip")) updater_skip();
  } else if (!strcmp(type, "window") && ds && win) {
    if (!strcmp(ds, "min")) gtk_window_iconify(GTK_WINDOW(win));
    else if (!strcmp(ds, "max")) toggle_maximize();
    else if (!strcmp(ds, "close")) close_main();
    else if (!strcmp(ds, "drag")) begin_drag();
  }
  g_free(ds);
  g_clear_object(&data);
out:
  g_free(type);
  g_clear_object(&m);
  g_free(raw);
}

// ---- notifications ----------------------------------------------------------------------------

static GIcon *icon_for(const char *name) {
  char *p = g_strdup_printf("%s/icons/%s.png", ui_dir, name);
  if (!g_file_test(p, G_FILE_TEST_EXISTS)) {
    g_free(p);
    p = g_strdup_printf("%s/icons/app.png", ui_dir);
  }
  GFile *f = g_file_new_for_path(p);
  GIcon *i = g_file_icon_new(f);
  g_object_unref(f);
  g_free(p);
  return i;
}

static void send_notification(const char *title, const char *body, const char *icon, const char *action, GVariant *target) {
  GNotification *n = g_notification_new(title);
  if (body && *body) g_notification_set_body(n, body);
  GIcon *i = icon_for(icon);
  g_notification_set_icon(n, i);
  g_object_unref(i);
  if (target) g_notification_set_default_action_and_target_value(n, action, target);
  else g_notification_set_default_action(n, action);
  char *id = g_strdup_printf("n%u", ++notify_counter);
  g_application_send_notification(G_APPLICATION(app), id, n);
  g_free(id);
  g_object_unref(n);
}

static gboolean window_focused(void) {
  return win && gtk_widget_get_visible(win) && gtk_window_is_active(GTK_WINDOW(win)) && !iconified;
}

static void notify_message(Svc *s, const char *title, const char *body) {
  if ((s == &svcs[0] && !cfg.notifyWa) || (s == &svcs[1] && !cfg.notifyTg)) return;
  gboolean focused = window_focused();
  if (focused && s == active_svc() && !settings_open) return;  // already looking at it
  if (app_locked) {  // locked: say that something arrived, not what
    title = s->name;
    body = "New message";
  }
  if (strcmp(cfg.notifyMode, "attention"))
    send_notification(title && *title ? title : s->name, body, s->id, "app.select", g_variant_new_string(s->id));
  if (strcmp(cfg.notifyMode, "toast") && !focused && win) gtk_window_set_urgency_hint(GTK_WINDOW(win), TRUE);
}

void app_notify_plain(const char *title, const char *body) { send_notification(title, body, "app", "app.settings", NULL); }

// ---- service pages --------------------------------------------------------------------------

static char *service_script(const char *svc, gboolean popup) {
  char **a = g_strsplit(service_js_src, "__SVC__", -1);
  char *s1 = g_strjoinv(svc, a);
  char **b = g_strsplit(s1, "__POPUP__", -1);
  char *s2 = g_strjoinv(popup ? "true" : "false", b);
  char *js = g_strconcat(
      "window.twincxtBridge = { postMessage: function (s) { window.webkit.messageHandlers.twincxtService.postMessage(String(s)); } };\n",
      s2, NULL);
  g_strfreev(a);
  g_strfreev(b);
  g_free(s1);
  g_free(s2);
  return js;
}

static void on_svc_msg(WebKitUserContentManager *u, WebKitJavascriptResult *r, ViewInfo *vi) {
  JSCValue *v = webkit_javascript_result_get_js_value(r);
  if (!jsc_value_is_string(v)) return;
  char *raw = jsc_value_to_string(v);
  JSCValue *m = json_parse(raw);
  char *type = jstr(m, "type");
  Svc *s = vi->svc;
  tw_log("%s%s: %.300s", s->id, vi->popup ? " popup" : "", raw);
  if (type && !strcmp(type, "frame") && !vi->popup) {
    int cr = (int)jnum(m, "r", 0), cg = (int)jnum(m, "g", 0), cb = (int)jnum(m, "b", 0);
    if (!s->has_strip || s->r != cr || s->g != cg || s->b != cb) {
      s->has_strip = TRUE;
      s->r = cr;
      s->g = cg;
      s->b = cb;
      if (s == active_svc()) apply_strip();
    }
  } else if (type && !strcmp(type, "notify")) {
    char *t = jstr(m, "title"), *b = jstr(m, "body");
    notify_message(s, t ? t : "", b ? b : "");
    g_free(t);
    g_free(b);
  }
  g_free(type);
  g_clear_object(&m);
  g_free(raw);
}

static WebKitUserContentManager *make_ucm(ViewInfo *vi) {
  WebKitUserContentManager *u = webkit_user_content_manager_new();
  char *js = service_script(vi->svc->id, vi->popup);
  WebKitUserScript *us = webkit_user_script_new(js, WEBKIT_USER_CONTENT_INJECT_ALL_FRAMES, WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START, NULL, NULL);
  webkit_user_content_manager_add_script(u, us);
  webkit_user_script_unref(us);
  g_free(js);
  g_signal_connect(u, "script-message-received::twincxtService", G_CALLBACK(on_svc_msg), vi);
  webkit_user_content_manager_register_script_message_handler(u, "twincxtService");
  return u;
}

static void popup_free(Popup *p);
static gboolean popup_free_idle(gpointer p) {
  popup_free(p);
  return G_SOURCE_REMOVE;
}

static gboolean on_decide_policy(WebKitWebView *v, WebKitPolicyDecision *d, WebKitPolicyDecisionType t, ViewInfo *vi) {
  if (t == WEBKIT_POLICY_DECISION_TYPE_RESPONSE) {
    WebKitResponsePolicyDecision *rd = WEBKIT_RESPONSE_POLICY_DECISION(d);
    if (webkit_response_policy_decision_is_main_frame_main_resource(rd) && !webkit_response_policy_decision_is_mime_type_supported(rd)) {
      webkit_policy_decision_download(d);
      return TRUE;
    }
    return FALSE;
  }
  WebKitNavigationAction *a = webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(d));
  const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(a));
  if (!uri) return FALSE;
  if (!g_ascii_strncasecmp(uri, "mailto:", 7) || !g_ascii_strncasecmp(uri, "tel:", 4)) {
    open_external(uri);
    webkit_policy_decision_ignore(d);
    return TRUE;
  }
  if (t == WEBKIT_POLICY_DECISION_TYPE_NEW_WINDOW_ACTION) {
    if (popupable(uri)) return FALSE;  // a call window: "create" follows
    open_external(uri);
    webkit_policy_decision_ignore(d);
    return TRUE;
  }
  if (is_web(uri) && !trusted(uri)) {
    Popup *p = vi->p;
    gboolean link = webkit_navigation_action_get_navigation_type(a) == WEBKIT_NAVIGATION_TYPE_LINK_CLICKED;
    if (link || (p && !p->loaded)) {
      open_external(uri);
      webkit_policy_decision_ignore(d);
      // An empty window opened only to show another site: not needed here.
      if (p && !p->loaded && !p->closing) {
        p->closing = TRUE;
        g_idle_add(popup_free_idle, p);
      }
      return TRUE;
    }
  }
  return FALSE;
}

static gboolean on_permission(WebKitWebView *v, WebKitPermissionRequest *r, ViewInfo *vi) {
  const char *uri = webkit_web_view_get_uri(v);
  gboolean ok = trusted(uri) || (vi->popup && (!uri || !*uri || g_str_has_prefix(uri, "about:")));
  if (WEBKIT_IS_NOTIFICATION_PERMISSION_REQUEST(r)) ok = FALSE;  // notifications go through Twincxt
  tw_log("%s: %s for %.80s: %s", vi->svc->id, G_OBJECT_TYPE_NAME(r), uri ? uri : "", ok ? "allowed" : "denied");
  if (ok) webkit_permission_request_allow(r);
  else webkit_permission_request_deny(r);
  return TRUE;
}

static void on_open_link(GSimpleAction *a, GVariant *param, gpointer d) { open_external(g_variant_get_string(param, NULL)); }

static gboolean on_context_menu(WebKitWebView *v, WebKitContextMenu *menu, GdkEvent *e, WebKitHitTestResult *hit, gpointer d) {
  GList *items = g_list_copy(webkit_context_menu_get_items(menu));
  for (GList *l = items; l; l = l->next) {
    WebKitContextMenuItem *it = l->data;
    switch (webkit_context_menu_item_get_stock_action(it)) {
      case WEBKIT_CONTEXT_MENU_ACTION_OPEN_LINK:
      case WEBKIT_CONTEXT_MENU_ACTION_OPEN_LINK_IN_NEW_WINDOW:
      case WEBKIT_CONTEXT_MENU_ACTION_DOWNLOAD_LINK_TO_DISK:
      case WEBKIT_CONTEXT_MENU_ACTION_OPEN_IMAGE_IN_NEW_WINDOW:
      case WEBKIT_CONTEXT_MENU_ACTION_COPY_IMAGE_URL_TO_CLIPBOARD:
      case WEBKIT_CONTEXT_MENU_ACTION_OPEN_FRAME_IN_NEW_WINDOW:
      case WEBKIT_CONTEXT_MENU_ACTION_OPEN_VIDEO_IN_NEW_WINDOW:
      case WEBKIT_CONTEXT_MENU_ACTION_OPEN_AUDIO_IN_NEW_WINDOW:
      case WEBKIT_CONTEXT_MENU_ACTION_COPY_VIDEO_LINK_TO_CLIPBOARD:
      case WEBKIT_CONTEXT_MENU_ACTION_COPY_AUDIO_LINK_TO_CLIPBOARD:
      case WEBKIT_CONTEXT_MENU_ACTION_GO_BACK:
      case WEBKIT_CONTEXT_MENU_ACTION_GO_FORWARD:
      case WEBKIT_CONTEXT_MENU_ACTION_STOP:
      case WEBKIT_CONTEXT_MENU_ACTION_RELOAD:
        webkit_context_menu_remove(menu, it);
        break;
      case WEBKIT_CONTEXT_MENU_ACTION_INSPECT_ELEMENT:
        if (!debug_on) webkit_context_menu_remove(menu, it);
        break;
      default:
        break;
    }
  }
  g_list_free(items);
  const char *link = webkit_hit_test_result_context_is_link(hit) ? webkit_hit_test_result_get_link_uri(hit) : NULL;
  if (is_web(link)) {
    webkit_context_menu_prepend(menu, webkit_context_menu_item_new_separator());
    webkit_context_menu_prepend(menu, webkit_context_menu_item_new_from_gaction(G_ACTION(open_link_action), "Open Link in Browser", g_variant_new_string(link)));
  }
  // No separators at the ends or next to each other.
  gboolean prev_sep = TRUE;
  items = g_list_copy(webkit_context_menu_get_items(menu));
  for (GList *l = items; l; l = l->next) {
    gboolean sep = webkit_context_menu_item_is_separator(l->data);
    if (sep && (prev_sep || !l->next)) webkit_context_menu_remove(menu, l->data);
    else prev_sep = sep;
  }
  g_list_free(items);
  items = webkit_context_menu_get_items(menu);
  GList *last = g_list_last(items);
  if (last && webkit_context_menu_item_is_separator(last->data)) webkit_context_menu_remove(menu, last->data);
  return webkit_context_menu_get_n_items(menu) == 0;  // TRUE: show nothing
}

static GtkWidget *on_create(WebKitWebView *v, WebKitNavigationAction *a, ViewInfo *vi);

static void connect_page(WebKitWebView *v, ViewInfo *vi) {
  g_signal_connect(v, "decide-policy", G_CALLBACK(on_decide_policy), vi);
  g_signal_connect(v, "permission-request", G_CALLBACK(on_permission), vi);
  g_signal_connect(v, "context-menu", G_CALLBACK(on_context_menu), vi);
  g_signal_connect(v, "create", G_CALLBACK(on_create), vi);
}

// ---- call windows ---------------------------------------------------------------------------

static Popup *popup_for_view(gpointer view) {
  for (GList *l = popups; l; l = l->next)
    if (((Popup *)l->data)->view == view) return l->data;
  return NULL;
}

static void popup_free(Popup *p) {
  if (p->dead) return;
  if (p->win) {  // its "destroy" handler comes back here
    GtkWidget *w = p->win;
    p->win = NULL;
    gtk_widget_destroy(w);
  }
  p->dead = TRUE;
  if (p->timer) g_source_remove(p->timer);
  popups = g_list_remove(popups, p);
  WebKitUserContentManager *u = webkit_web_view_get_user_content_manager(p->view);
  if (u) g_signal_handlers_disconnect_by_data(u, &p->info);
  g_signal_handlers_disconnect_by_data(p->view, &p->info);
  g_signal_handlers_disconnect_by_data(p->view, p);
  gtk_widget_destroy(GTK_WIDGET(p->view));
  g_object_unref(p->view);
  tw_log("popup: closed (%u open)", g_list_length(popups));
  g_free(p);
}

static void on_popup_destroyed(GtkWidget *w, Popup *p) {
  if (p->win != w) return;
  p->win = NULL;
  popup_free(p);
}

static gboolean popup_timeout(gpointer data) {
  Popup *p = data;
  p->timer = 0;
  popup_free(p);
  return G_SOURCE_REMOVE;
}

static void on_move_here(GObject *src, GAsyncResult *res, gpointer view) {
  GError *err = NULL;
  gboolean found = FALSE;
  WebKitJavascriptResult *r = webkit_web_view_run_javascript_finish(WEBKIT_WEB_VIEW(src), res, &err);
  if (r) {
    found = jsc_value_to_boolean(webkit_javascript_result_get_js_value(r));
    webkit_javascript_result_unref(r);
  } else g_error_free(err);
  tw_log("popup: WhatsApp's \"Move here\" %s", found ? "clicked" : "not found");
  Popup *p = popup_for_view(view);
  if (p && !found) popup_free(p);
  g_object_unref(view);
}

// Closing the call window hands the call back to the main window through
// WhatsApp's own "Move here". Until WhatsApp has done so the window stays open
// for the page but invisible to you, so the camera keeps running.
static gboolean on_popup_delete(GtkWidget *w, GdkEvent *e, Popup *p) {
  if (p->closing || strcmp(p->info.svc->id, "wa") || !svcs[0].view || quitting) return FALSE;
  p->closing = TRUE;
  gtk_widget_set_opacity(w, 0);
  cairo_region_t *none = cairo_region_create();
  gtk_widget_input_shape_combine_region(w, none);
  cairo_region_destroy(none);
  gtk_window_set_skip_taskbar_hint(GTK_WINDOW(w), TRUE);
  gtk_window_set_skip_pager_hint(GTK_WINDOW(w), TRUE);
#ifdef GDK_WINDOWING_X11
  if (GDK_IS_X11_DISPLAY(gtk_widget_get_display(w))) gtk_window_move(GTK_WINDOW(w), -32000, -32000);
#endif
  tw_log("popup: closed by you, asking WhatsApp to move the call back");
  webkit_web_view_run_javascript(svcs[0].view, MOVE_HERE_JS, NULL, on_move_here, g_object_ref(p->view));
  p->timer = g_timeout_add(3000, popup_timeout, p);
  return TRUE;
}

static void on_popup_ready(WebKitWebView *v, Popup *p) {
  if (p->dead || p->win) return;
  GdkRectangle r = {0};
  webkit_window_properties_get_geometry(webkit_web_view_get_window_properties(v), &r);
  p->win = gtk_window_new(GTK_WINDOW_TOPLEVEL);
  gtk_window_set_application(GTK_WINDOW(p->win), app);
  gtk_window_set_title(GTK_WINDOW(p->win), p->info.svc->name);
  gtk_window_set_default_size(GTK_WINDOW(p->win), r.width >= 200 ? r.width : 960, r.height >= 200 ? r.height : 640);
  gtk_widget_set_size_request(GTK_WIDGET(v), 360, 300);
  gtk_container_add(GTK_CONTAINER(p->win), GTK_WIDGET(v));
  g_signal_connect(p->win, "delete-event", G_CALLBACK(on_popup_delete), p);
  g_signal_connect(p->win, "destroy", G_CALLBACK(on_popup_destroyed), p);
  gtk_widget_show_all(p->win);
  tw_log("popup: shown for %s (%dx%d)", p->info.svc->id, r.width, r.height);
}

static void on_popup_close(WebKitWebView *v, Popup *p) {
  tw_log("popup: closed by the page");
  popup_free(p);
}

static void on_popup_load(WebKitWebView *v, WebKitLoadEvent e, Popup *p) {
  if (e == WEBKIT_LOAD_COMMITTED && trusted(webkit_web_view_get_uri(v))) p->loaded = TRUE;
}

static GtkWidget *on_create(WebKitWebView *v, WebKitNavigationAction *a, ViewInfo *vi) {
  const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(a));
  if (!popupable(uri)) {
    open_external(uri);
    return NULL;
  }
  Popup *p = g_new0(Popup, 1);
  p->info.svc = vi->svc;
  p->info.popup = TRUE;
  p->info.p = p;
  WebKitUserContentManager *u = make_ucm(&p->info);
  p->view = WEBKIT_WEB_VIEW(g_object_new(WEBKIT_TYPE_WEB_VIEW, "related-view", v, "user-content-manager", u, "settings", webkit_web_view_get_settings(v), NULL));
  g_object_unref(u);
  g_object_ref_sink(p->view);
  connect_page(p->view, &p->info);
  g_signal_connect(p->view, "ready-to-show", G_CALLBACK(on_popup_ready), p);
  g_signal_connect(p->view, "close", G_CALLBACK(on_popup_close), p);
  g_signal_connect(p->view, "load-changed", G_CALLBACK(on_popup_load), p);
  popups = g_list_append(popups, p);
  tw_log("popup: opened for %s (%.120s)", vi->svc->id, uri ? uri : "");
  return GTK_WIDGET(p->view);
}

// ---- service views ------------------------------------------------------------------------------

static void on_title(WebKitWebView *v, GParamSpec *ps, Svc *s) {
  // "(3) WhatsApp", "Telegram (3)": the first "(number)" in the title.
  const char *t = webkit_web_view_get_title(v);
  int n = 0;
  for (const char *o = t ? strchr(t, '(') : NULL; o; o = strchr(o + 1, '(')) {
    if (!g_ascii_isdigit(o[1])) continue;
    char *end = NULL;
    long x = strtol(o + 1, &end, 10);
    if (end && *end == ')') {
      n = (int)CLAMP(x, 0, 1000000);
      break;
    }
  }
  if (n != s->unread) {
    s->unread = n;
    push_unread(s);
  }
}

static void on_load(WebKitWebView *v, WebKitLoadEvent e, Svc *s) {
  if (e == WEBKIT_LOAD_STARTED) {
    if (trusted(webkit_web_view_get_uri(v))) {
      s->ready = FALSE;
      push_loading(s, TRUE);
    }
  } else if (e == WEBKIT_LOAD_FINISHED) {
    s->ready = TRUE;
    push_loading(s, FALSE);
    webkit_web_view_set_zoom_level(v, (s == &svcs[0] ? cfg.zoomWa : cfg.zoomTg) / 100.0);
    apply_privacy(s);
    tw_log("%s: loaded %.120s", s->id, webkit_web_view_get_uri(v));
  }
}

static void on_crashed(WebKitWebView *v, WebKitWebProcessTerminationReason reason, Svc *s) {
  tw_log("%s: web process gone (%d)", s->id, reason);
  if (reason != WEBKIT_WEB_PROCESS_TERMINATED_BY_API) reload_service(s);
}

static gboolean on_enter_fullscreen(WebKitWebView *v, Svc *s) {
  full_svc = s;
  layout();
  return FALSE;  // WebKit makes the window full screen
}

static gboolean on_leave_fullscreen(WebKitWebView *v, Svc *s) {
  full_svc = NULL;
  layout();
  return FALSE;
}

static void create_service(Svc *s) {
  s->info.svc = s;
  WebKitUserContentManager *u = make_ucm(&s->info);
  s->view = WEBKIT_WEB_VIEW(g_object_new(WEBKIT_TYPE_WEB_VIEW, "web-context", wctx, "user-content-manager", u, "settings", svc_settings, NULL));
  g_object_unref(u);
  connect_page(s->view, &s->info);
  g_signal_connect(s->view, "notify::title", G_CALLBACK(on_title), s);
  g_signal_connect(s->view, "load-changed", G_CALLBACK(on_load), s);
  g_signal_connect(s->view, "web-process-terminated", G_CALLBACK(on_crashed), s);
  g_signal_connect(s->view, "enter-fullscreen", G_CALLBACK(on_enter_fullscreen), s);
  g_signal_connect(s->view, "leave-fullscreen", G_CALLBACK(on_leave_fullscreen), s);
  webkit_web_view_set_zoom_level(s->view, (s == &svcs[0] ? cfg.zoomWa : cfg.zoomTg) / 100.0);
  gtk_widget_set_halign(GTK_WIDGET(s->view), GTK_ALIGN_FILL);
  gtk_widget_set_valign(GTK_WIDGET(s->view), GTK_ALIGN_FILL);
  gtk_overlay_add_overlay(GTK_OVERLAY(overlay), GTK_WIDGET(s->view));
  reload_service(s);
}

// ---- downloads ------------------------------------------------------------------------------------

static gboolean on_decide_destination(WebKitDownload *d, const char *suggested, gpointer data) {
  const char *dir = g_get_user_special_dir(G_USER_DIRECTORY_DOWNLOAD);
  char *fallback = NULL;
  if (!dir) dir = fallback = g_build_filename(g_get_home_dir(), "Downloads", NULL);
  g_mkdir_with_parents(dir, 0755);
  char *name = g_path_get_basename(suggested && *suggested ? suggested : "download");
  if (!strcmp(name, ".") || !strcmp(name, "/") || !strcmp(name, "..")) str_set(&name, "download");
  const char *dot = strrchr(name, '.');
  char *base = dot && dot != name ? g_strndup(name, (gsize)(dot - name)) : g_strdup(name);
  const char *ext = dot && dot != name ? dot : "";
  char *target = g_build_filename(dir, name, NULL);
  for (int i = 1; g_file_test(target, G_FILE_TEST_EXISTS); i++) {
    g_free(target);
    char *n = g_strdup_printf("%s (%d)%s", base, i, ext);
    target = g_build_filename(dir, n, NULL);
    g_free(n);
  }
  char *uri = g_filename_to_uri(target, NULL, NULL);
  webkit_download_set_allow_overwrite(d, FALSE);
  webkit_download_set_destination(d, uri);
  g_object_set_data_full(G_OBJECT(d), "tw-path", target, g_free);
  tw_log("download: %s", target);
  g_free(uri);
  g_free(base);
  g_free(name);
  g_free(fallback);
  return TRUE;
}

static void on_download_failed(WebKitDownload *d, GError *err, gpointer data) {
  g_object_set_data(G_OBJECT(d), "tw-failed", GINT_TO_POINTER(1));
  tw_log("download failed: %s", err ? err->message : "");
}

static void on_download_finished(WebKitDownload *d, gpointer data) {
  const char *path = g_object_get_data(G_OBJECT(d), "tw-path");
  if (!path || g_object_get_data(G_OBJECT(d), "tw-failed")) return;
  char *base = g_path_get_basename(path);
  send_notification("Download complete", base, "app", "app.show-file", g_variant_new_string(path));
  g_free(base);
}

static void on_download_started(WebKitWebContext *c, WebKitDownload *d, gpointer data) {
  g_signal_connect(d, "decide-destination", G_CALLBACK(on_decide_destination), NULL);
  g_signal_connect(d, "failed", G_CALLBACK(on_download_failed), NULL);
  g_signal_connect(d, "finished", G_CALLBACK(on_download_finished), NULL);
}

// ---- keyboard ---------------------------------------------------------------------------------------

// Shortcuts while any page has focus: Alt+1 / Alt+2, Ctrl+Tab, Ctrl+, (settings),
// Ctrl+L (lock), Ctrl+= / Ctrl+- / Ctrl+0 (zoom), Ctrl+Q (quit).
static gboolean on_key(GtkWidget *w, GdkEventKey *e, gpointer d) {
  GdkModifierType mods = e->state & gtk_accelerator_get_default_mod_mask();
  guint k = gdk_keyval_to_lower(e->keyval);
  gboolean ctrl = (mods & ~GDK_SHIFT_MASK) == GDK_CONTROL_MASK;
  gboolean tab = k == GDK_KEY_Tab || k == GDK_KEY_ISO_Left_Tab || k == GDK_KEY_KP_Tab;
  if (mods == GDK_CONTROL_MASK && k == GDK_KEY_l) {
    app_lock_now();
    return TRUE;
  }
  if (app_locked) {  // the lock screen: no switching, no settings
    if (mods == GDK_CONTROL_MASK && k == GDK_KEY_q) {
      app_quit();
      return TRUE;
    }
    return (mods == GDK_MOD1_MASK && (k == GDK_KEY_1 || k == GDK_KEY_2 || k == GDK_KEY_KP_1 || k == GDK_KEY_KP_2)) ||
           ((mods & GDK_CONTROL_MASK) && (tab || k == GDK_KEY_comma));
  }
  if ((mods & GDK_CONTROL_MASK) && (k == GDK_KEY_Tab || k == GDK_KEY_ISO_Left_Tab || k == GDK_KEY_KP_Tab))
    select_service(active_svc() == &svcs[0] ? "tg" : "wa", "Ctrl+Tab");
  else if (mods == GDK_MOD1_MASK && (k == GDK_KEY_1 || k == GDK_KEY_KP_1)) select_service("wa", "shortcut");
  else if (mods == GDK_MOD1_MASK && (k == GDK_KEY_2 || k == GDK_KEY_KP_2)) select_service("tg", "shortcut");
  else if (mods == GDK_CONTROL_MASK && k == GDK_KEY_comma) toggle_settings(-1);
  else if (ctrl && (k == GDK_KEY_equal || k == GDK_KEY_plus || k == GDK_KEY_KP_Add)) zoom_by(1);
  else if (ctrl && (k == GDK_KEY_minus || k == GDK_KEY_KP_Subtract)) zoom_by(-1);
  else if (mods == GDK_CONTROL_MASK && (k == GDK_KEY_0 || k == GDK_KEY_KP_0)) zoom_by(0);
  else if (mods == GDK_CONTROL_MASK && k == GDK_KEY_q) app_quit();
  else return FALSE;
  return TRUE;
}

// ---- system theme and window buttons ----------------------------------------------------------------

static gboolean color_scheme_dark(GVariant *v) {
  GVariant *cur = g_variant_ref(v);
  while (g_variant_is_of_type(cur, G_VARIANT_TYPE_VARIANT)) {
    GVariant *in = g_variant_get_variant(cur);
    g_variant_unref(cur);
    cur = in;
  }
  gboolean dark = g_variant_is_of_type(cur, G_VARIANT_TYPE_UINT32) && g_variant_get_uint32(cur) == 1;
  g_variant_unref(cur);
  return dark;
}

static void set_system_dark(gboolean dark) {
  if (dark == system_dark) return;
  system_dark = dark;
  tw_log("system theme: %s", dark ? "dark" : "light");
  apply_theme();
  push_state();
}

static void on_portal_read(GObject *src, GAsyncResult *res, gpointer d) {
  GError *err = NULL;
  GVariant *r = g_dbus_connection_call_finish(G_DBUS_CONNECTION(src), res, &err);
  if (!r) {
    g_error_free(err);
    return;  // no portal: the GTK theme name decides (see read_system_theme)
  }
  GVariant *v = NULL;
  g_variant_get(r, "(v)", &v);
  set_system_dark(color_scheme_dark(v));
  g_variant_unref(v);
  g_variant_unref(r);
}

static void on_portal_changed(GDBusConnection *c, const char *sender, const char *path, const char *iface, const char *signal, GVariant *params, gpointer d) {
  const char *ns = NULL, *key = NULL;
  GVariant *v = NULL;
  g_variant_get(params, "(&s&sv)", &ns, &key, &v);
  if (!g_strcmp0(ns, "org.freedesktop.appearance") && !g_strcmp0(key, "color-scheme")) set_system_dark(color_scheme_dark(v));
  g_variant_unref(v);
}

static void read_system_theme(void) {
  char *theme = NULL;
  g_object_get(gtk_settings_get_default(), "gtk-theme-name", &theme, NULL);
  system_dark = theme && g_str_has_suffix(theme, "-dark");
  g_free(theme);
  GDBusConnection *bus = g_application_get_dbus_connection(G_APPLICATION(app));
  if (!bus) bus = g_bus_get_sync(G_BUS_TYPE_SESSION, NULL, NULL);
  else g_object_ref(bus);
  if (!bus) return;
  g_dbus_connection_signal_subscribe(bus, "org.freedesktop.portal.Desktop", "org.freedesktop.portal.Settings", "SettingChanged",
                                     "/org/freedesktop/portal/desktop", NULL, G_DBUS_SIGNAL_FLAGS_NONE, on_portal_changed, NULL, NULL);
  g_dbus_connection_call(bus, "org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop", "org.freedesktop.portal.Settings", "Read",
                         g_variant_new("(ss)", "org.freedesktop.appearance", "color-scheme"), G_VARIANT_TYPE("(v)"), G_DBUS_CALL_FLAGS_NONE, 3000,
                         NULL, on_portal_read, NULL);
  g_object_unref(bus);
}

static void read_decoration_layout(void) {
  char *l = NULL;
  g_object_get(gtk_settings_get_default(), "gtk-decoration-layout", &l, NULL);
  g_free(decoration_layout);
  decoration_layout = l ? l : g_strdup("menu:minimize,maximize,close");
}

static void on_decoration_layout(GObject *o, GParamSpec *ps, gpointer d) {
  read_decoration_layout();
  tw_log("window buttons: %s", decoration_layout);
  push_state();
}

// ---- test hooks ---------------------------------------------------------------------------------------

static char *shot_path(const char *suffix) {
  const char *p = g_getenv("TWINCXT_SMOKE_SHOT");
  if (!p || !*p) return NULL;
  if (!suffix) return g_strdup(p);
  if (g_str_has_suffix(p, ".png")) return g_strdup_printf("%.*s-%s.png", (int)strlen(p) - 4, p, suffix);
  return g_strdup_printf("%s-%s.png", p, suffix);
}

static void save_shot(const char *suffix) {
  char *path = shot_path(suffix);
  if (!path || !win) {
    g_free(path);
    return;
  }
  GdkWindow *gw = gtk_widget_get_window(win);
  GdkPixbuf *pb = gw ? gdk_pixbuf_get_from_window(gw, 0, 0, gdk_window_get_width(gw), gdk_window_get_height(gw)) : NULL;
  GError *err = NULL;
  if (pb && gdk_pixbuf_save(pb, path, "png", &err, NULL)) tw_log("smoke: screenshot %s", path);
  else {
    tw_log("smoke: screenshot failed: %s", err ? err->message : "no window");
    g_clear_error(&err);
  }
  g_clear_object(&pb);
  g_free(path);
}

static const char *PROBE_JS =
    "JSON.stringify({url: location.href, title: document.title,"
    " RTCPeerConnection: typeof RTCPeerConnection,"
    " mediaDevices: !!navigator.mediaDevices,"
    " getUserMedia: !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia),"
    " MediaRecorder: typeof MediaRecorder, serviceWorker: !!navigator.serviceWorker,"
    " userAgent: navigator.userAgent,"
    " text: ((document.body && document.body.innerText) || '').replace(/\\s+/g, ' ').slice(0, 400)})";

static void on_probe(GObject *src, GAsyncResult *res, gpointer name) {
  GError *err = NULL;
  WebKitJavascriptResult *r = webkit_web_view_run_javascript_finish(WEBKIT_WEB_VIEW(src), res, &err);
  if (r) {
    char *s = jsc_value_to_string(webkit_javascript_result_get_js_value(r));
    printf("SMOKE %s: %s\n", (const char *)name, s);
    fflush(stdout);
    tw_log("smoke %s: %s", (const char *)name, s);
    g_free(s);
    webkit_javascript_result_unref(r);
  } else {
    printf("SMOKE %s: error %s\n", (const char *)name, err->message);
    fflush(stdout);
    g_error_free(err);
  }
}

static int smoke_step, smoke_lock_step;
static gboolean smoke_next(gpointer d) {
  // Started locked (CI writes a config with the PIN 1234): a wrong PIN, a
  // picture of the lock screen, then the right PIN.
  if (smoke_step == 0 && (app_locked || smoke_lock_step)) {
    switch (smoke_lock_step++) {
      case 0:
        shell_eval("twincxtHost.postMessage(JSON.stringify({type:'lock',data:{op:'unlock',pin:'0000'}}))");
        g_timeout_add_seconds(2, smoke_next, NULL);
        return G_SOURCE_REMOVE;
      case 1:
        save_shot("locked");
        shell_eval("twincxtHost.postMessage(JSON.stringify({type:'lock',data:{op:'unlock',pin:'1234'}}))");
        g_timeout_add_seconds(4, smoke_next, NULL);
        return G_SOURCE_REMOVE;
      default:
        printf("SMOKE lock: %s after the right PIN\n", app_locked ? "still locked" : "unlocked");
        fflush(stdout);
        smoke_lock_step = 0;
        if (app_locked) {
          app_quit();
          return G_SOURCE_REMOVE;
        }
    }
  }
  switch (smoke_step++) {
    case 0:
      webkit_web_view_run_javascript(svcs[0].view, PROBE_JS, NULL, on_probe, (gpointer) "wa");
      webkit_web_view_run_javascript(svcs[1].view, PROBE_JS, NULL, on_probe, (gpointer) "tg");
      save_shot(NULL);
      select_service("tg", "smoke");
      g_timeout_add_seconds(6, smoke_next, NULL);
      break;
    case 1:
      save_shot("tg");
      toggle_settings(1);
      g_timeout_add_seconds(2, smoke_next, NULL);
      break;
    default:
      save_shot("settings");
      toggle_settings(0);
      select_service("wa", "smoke");
      printf("SMOKE done\n");
      fflush(stdout);
      app_quit();
  }
  return G_SOURCE_REMOVE;
}

// ---- start ------------------------------------------------------------------------------------------------

static void init_paths(void) {
  const char *u = g_getenv("TWINCXT_USERDATA");
  if (u && *u) {
    config_dir = g_strdup(u);
    data_dir = g_build_filename(u, "data", NULL);
    cache_dir = g_build_filename(u, "cache", NULL);
  } else {
    config_dir = g_build_filename(g_get_user_config_dir(), "Twincxt", NULL);
    data_dir = g_build_filename(g_get_user_data_dir(), "Twincxt", NULL);
    cache_dir = g_build_filename(g_get_user_cache_dir(), "Twincxt", NULL);
  }
  config_path = g_build_filename(config_dir, "config.json", NULL);
  log_path = g_build_filename(config_dir, "debug.log", NULL);
  updates_dir = g_build_filename(cache_dir, "updates", NULL);
  g_mkdir_with_parents(config_dir, 0700);
  g_mkdir_with_parents(data_dir, 0700);
  g_mkdir_with_parents(cache_dir, 0700);
}

static char *find_ui_dir(void) {
  char *exe = g_file_read_link("/proc/self/exe", NULL);
  char *dir = exe ? g_path_get_dirname(exe) : g_strdup(".");
  char *cands[] = {
      g_strdup(g_getenv("TWINCXT_UI")),
      g_build_filename(dir, "ui", NULL),                          // AppImage, dev
      g_build_filename(dir, "..", "share", "twincxt", "ui", NULL),  // /usr/bin -> /usr/share
      g_build_filename(dir, "..", "..", "ui", NULL),                // linux/build -> repo
      g_strdup("/usr/share/twincxt/ui"),
  };
  char *found = NULL;
  for (guint i = 0; i < G_N_ELEMENTS(cands); i++) {
    if (!found && cands[i]) {
      char *f = g_build_filename(cands[i], "shell.html", NULL);
      if (g_file_test(f, G_FILE_TEST_EXISTS)) found = g_canonicalize_filename(cands[i], NULL);
      g_free(f);
    }
    g_free(cands[i]);
  }
  g_free(dir);
  g_free(exe);
  return found;
}

// WebKitGTK 2.42+ lists its feature flags; the WebRTC ones are switched on
// here (and logged). Looked up at run time so 2.40 still works.
static void enable_rtc_features(void) {
  typedef gpointer (*all_fn)(void);
  typedef gsize (*len_fn)(gpointer);
  typedef gpointer (*get_fn)(gpointer, gsize);
  typedef const char *(*id_fn)(gpointer);
  typedef gboolean (*def_fn)(gpointer);
  typedef void (*set_fn)(WebKitSettings *, gpointer, gboolean);
  all_fn all = (all_fn)dlsym(RTLD_DEFAULT, "webkit_settings_get_all_features");
  len_fn len = (len_fn)dlsym(RTLD_DEFAULT, "webkit_feature_list_get_length");
  get_fn get = (get_fn)dlsym(RTLD_DEFAULT, "webkit_feature_list_get");
  id_fn ident = (id_fn)dlsym(RTLD_DEFAULT, "webkit_feature_get_identifier");
  def_fn def = (def_fn)dlsym(RTLD_DEFAULT, "webkit_feature_get_default_value");
  set_fn set = (set_fn)dlsym(RTLD_DEFAULT, "webkit_settings_set_feature_enabled");
  if (!all || !len || !get || !ident || !def || !set) return;
  gpointer list = all();
  GString *seen = g_string_new(NULL);
  for (gsize i = 0; list && i < len(list); i++) {
    gpointer f = get(list, i);
    const char *id = ident(f);
    if (!id || !(strstr(id, "RTC") || strstr(id, "PeerConnection") || strstr(id, "MediaStream") || strstr(id, "MediaDevices"))) continue;
    g_string_append_printf(seen, " %s(%s)", id, def(f) ? "on" : "off");
    if (strstr(id, "PeerConnection")) set(svc_settings, f, TRUE);
  }
  tw_log("WebRTC features:%s", seen->len ? seen->str : " none (built without WebRTC)");
  g_string_free(seen, TRUE);
}

static void setup_web(void) {
  WebKitWebsiteDataManager *mgr = webkit_website_data_manager_new("base-data-directory", data_dir, "base-cache-directory", cache_dir, NULL);
  wctx = webkit_web_context_new_with_website_data_manager(mgr);
  WebKitCookieManager *cm = webkit_web_context_get_cookie_manager(wctx);
  char *cookies = g_build_filename(data_dir, "cookies.sqlite", NULL);
  webkit_cookie_manager_set_persistent_storage(cm, cookies, WEBKIT_COOKIE_PERSISTENT_STORAGE_SQLITE);
  webkit_cookie_manager_set_accept_policy(cm, WEBKIT_COOKIE_POLICY_ACCEPT_ALWAYS);
  g_free(cookies);
  g_object_unref(mgr);
  webkit_web_context_set_cache_model(wctx, WEBKIT_CACHE_MODEL_WEB_BROWSER);
  webkit_web_context_set_spell_checking_enabled(wctx, TRUE);
  const char *const *langs = g_get_language_names();
  for (int i = 0; langs[i]; i++) {
    if (strlen(langs[i]) == 5 && langs[i][2] == '_') {  // e.g. en_US
      const char *one[] = {langs[i], NULL};
      webkit_web_context_set_spell_checking_languages(wctx, one);
      break;
    }
  }
  g_signal_connect(wctx, "download-started", G_CALLBACK(on_download_started), NULL);

  user_agent = make_user_agent();
  svc_settings = webkit_settings_new();
  webkit_settings_set_user_agent(svc_settings, user_agent);
  webkit_settings_set_enable_media_stream(svc_settings, TRUE);
  webkit_settings_set_enable_mediasource(svc_settings, TRUE);
  webkit_settings_set_enable_encrypted_media(svc_settings, TRUE);
  webkit_settings_set_javascript_can_open_windows_automatically(svc_settings, TRUE);
  webkit_settings_set_media_playback_requires_user_gesture(svc_settings, FALSE);
  webkit_settings_set_enable_smooth_scrolling(svc_settings, TRUE);
  webkit_settings_set_enable_developer_extras(svc_settings, debug_on);
  GObjectClass *k = G_OBJECT_GET_CLASS(svc_settings);
  const char *optional[] = {"enable-webrtc", "enable-media-capabilities", "enable-media", NULL};
  for (int i = 0; optional[i]; i++)
    if (g_object_class_find_property(k, optional[i])) g_object_set(svc_settings, optional[i], TRUE, NULL);
  tw_log("WebKitGTK %u.%u.%u, user agent %s, enable-webrtc %s", webkit_get_major_version(), webkit_get_minor_version(), webkit_get_micro_version(),
         user_agent, g_object_class_find_property(k, "enable-webrtc") ? "set" : "not available");
  enable_rtc_features();

  char *js = NULL;
  char *path = g_build_filename(ui_dir, "service.js", NULL);
  if (!g_file_get_contents(path, &js, NULL, NULL)) js = g_strdup("");
  service_js_src = js;
  g_free(path);
}

static void create_shell(void) {
  WebKitUserContentManager *u = webkit_user_content_manager_new();
  WebKitUserScript *us = webkit_user_script_new(
      "window.twincxtHost = { postMessage: function (s) { window.webkit.messageHandlers.twincxt.postMessage(String(s)); } };",
      WEBKIT_USER_CONTENT_INJECT_TOP_FRAME, WEBKIT_USER_SCRIPT_INJECT_AT_DOCUMENT_START, NULL, NULL);
  webkit_user_content_manager_add_script(u, us);
  webkit_user_script_unref(us);
  g_signal_connect(u, "script-message-received::twincxt", G_CALLBACK(on_shell_msg), NULL);
  webkit_user_content_manager_register_script_message_handler(u, "twincxt");
  WebKitSettings *st = webkit_settings_new();
  webkit_settings_set_enable_developer_extras(st, debug_on);
  webkit_settings_set_enable_back_forward_navigation_gestures(st, FALSE);
  shell = WEBKIT_WEB_VIEW(g_object_new(WEBKIT_TYPE_WEB_VIEW, "web-context", wctx, "user-content-manager", u, "settings", st, NULL));
  g_object_unref(u);
  g_object_unref(st);
  g_signal_connect(shell, "button-press-event", G_CALLBACK(on_shell_press), NULL);
  if (!debug_on) g_signal_connect(shell, "context-menu", G_CALLBACK(gtk_true), NULL);
  char *file = g_build_filename(ui_dir, "shell.html", NULL);
  char *uri = g_filename_to_uri(file, NULL, NULL);
  webkit_web_view_load_uri(shell, uri);
  g_free(uri);
  g_free(file);
}

static gboolean on_shell_decide(WebKitWebView *v, WebKitPolicyDecision *d, WebKitPolicyDecisionType t, gpointer data) {
  if (t != WEBKIT_POLICY_DECISION_TYPE_RESPONSE) {
    WebKitNavigationAction *a = webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(d));
    const char *uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(a));
    if (!uri || !g_str_has_prefix(uri, "file:")) {  // the shell never leaves shell.html
      if (webkit_navigation_action_is_user_gesture(a)) open_external(uri);
      webkit_policy_decision_ignore(d);
      return TRUE;
    }
  }
  return FALSE;
}

static void set_icons(void) {
  GtkIconTheme *t = gtk_icon_theme_get_default();
  if (gtk_icon_theme_has_icon(t, "twincxt")) {
    gtk_window_set_default_icon_name("twincxt");
    return;
  }
  char *p = g_build_filename(ui_dir, "icons", "app.png", NULL);
  gtk_window_set_default_icon_from_file(p, NULL);
  g_free(p);
}

static void create_window(gboolean autostarted) {
  set_icons();
  win = gtk_application_window_new(app);
  gtk_window_set_title(GTK_WINDOW(win), "Twincxt");
  gtk_window_set_default_size(GTK_WINDOW(win), MAX(MIN_W, cfg.ww), MAX(MIN_H, cfg.wh));
  normal_w = MAX(MIN_W, cfg.ww);
  normal_h = MAX(MIN_H, cfg.wh);
  // Client-side decorations with an empty title bar: the window manager draws
  // none, and the shell's title strip (with GNOME's buttons) is the header bar.
  GtkWidget *bar = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 0);
  gtk_style_context_add_class(gtk_widget_get_style_context(bar), "tw-empty");
  gtk_widget_show(bar);
  gtk_window_set_titlebar(GTK_WINDOW(win), bar);

  // Adwaita styles any .titlebar like a header bar (46 px high); this one is empty.
  GtkCssProvider *css = gtk_css_provider_new();
  gtk_css_provider_load_from_data(css,
                                  ".titlebar.tw-empty { min-height: 0; padding: 0; margin: 0; border: none; box-shadow: none; background: none; }"
                                  " window.twincxt { background-color: #EBEBEB; }",
                                  -1, NULL);
  gtk_style_context_add_provider_for_screen(gdk_screen_get_default(), GTK_STYLE_PROVIDER(css), GTK_STYLE_PROVIDER_PRIORITY_APPLICATION);
  g_object_unref(css);
  gtk_style_context_add_class(gtk_widget_get_style_context(win), "twincxt");

  overlay = gtk_overlay_new();
  gtk_widget_set_size_request(overlay, MIN_W, MIN_H);
  gtk_container_add(GTK_CONTAINER(win), overlay);
  create_shell();
  g_signal_connect(shell, "decide-policy", G_CALLBACK(on_shell_decide), NULL);
  gtk_container_add(GTK_CONTAINER(overlay), GTK_WIDGET(shell));
  for (int i = 0; i < 2; i++) create_service(&svcs[i]);
  apply_theme();

  g_signal_connect(win, "delete-event", G_CALLBACK(on_delete), NULL);
  g_signal_connect(win, "window-state-event", G_CALLBACK(on_window_state), NULL);
  g_signal_connect(win, "configure-event", G_CALLBACK(on_configure), NULL);
  g_signal_connect(win, "focus-in-event", G_CALLBACK(on_focus_in), NULL);
  g_signal_connect(win, "focus-out-event", G_CALLBACK(on_focus_out), NULL);
  g_signal_connect(win, "key-press-event", G_CALLBACK(on_key), NULL);

  gtk_widget_show_all(overlay);
  layout();
  if (cfg.wmax) gtk_window_maximize(GTK_WINDOW(win));
  if (autostarted && cfg.startHidden) {
    gtk_widget_realize(win);
    if (!tray_available()) {
      gtk_window_iconify(GTK_WINDOW(win));
      gtk_widget_show(win);
    }
  } else {
    gtk_widget_show(win);
    gtk_window_present(GTK_WINDOW(win));
  }
  focus_active();
}

static void act_select(GSimpleAction *a, GVariant *p, gpointer d) {
  select_service(g_variant_get_string(p, NULL), "notification");
  app_show_window();
}
static void act_settings(GSimpleAction *a, GVariant *p, gpointer d) { app_open_settings(); }
static void act_show(GSimpleAction *a, GVariant *p, gpointer d) { app_show_window(); }
static void act_quit(GSimpleAction *a, GVariant *p, gpointer d) { app_quit(); }

static void open_folder_of(const char *path) {
  char *dir = g_path_get_dirname(path);
  char *uri = g_filename_to_uri(dir, NULL, NULL);
  if (uri) gtk_show_uri_on_window(NULL, uri, GDK_CURRENT_TIME, NULL);
  g_free(uri);
  g_free(dir);
}

static void on_shown_in_folder(GObject *src, GAsyncResult *res, gpointer path) {
  GError *err = NULL;
  GVariant *r = g_dbus_connection_call_finish(G_DBUS_CONNECTION(src), res, &err);
  if (r) g_variant_unref(r);
  else {  // no file manager service: open the folder
    g_error_free(err);
    open_folder_of(path);
  }
  g_free(path);
}

static void act_show_file(GSimpleAction *a, GVariant *p, gpointer d) {
  const char *path = g_variant_get_string(p, NULL);
  char *uri = g_filename_to_uri(path, NULL, NULL);
  GDBusConnection *bus = g_application_get_dbus_connection(G_APPLICATION(app));
  if (uri && bus) {
    const char *uris[] = {uri, NULL};
    g_dbus_connection_call(bus, "org.freedesktop.FileManager1", "/org/freedesktop/FileManager1", "org.freedesktop.FileManager1", "ShowItems",
                           g_variant_new("(^ass)", uris, ""), NULL, G_DBUS_CALL_FLAGS_NONE, 5000, NULL, on_shown_in_folder, g_strdup(path));
  } else {
    open_folder_of(path);
  }
  g_free(uri);
}

static void on_startup(GApplication *a, gpointer d) {
  static const GActionEntry actions[] = {
      {"select", act_select, "s", NULL, NULL, {0}},
      {"settings", act_settings, NULL, NULL, NULL, {0}},
      {"show", act_show, NULL, NULL, NULL, {0}},
      {"show-file", act_show_file, "s", NULL, NULL, {0}},
      {"quit", act_quit, NULL, NULL, NULL, {0}},
  };
  g_action_map_add_action_entries(G_ACTION_MAP(a), actions, G_N_ELEMENTS(actions), NULL);
  open_link_action = g_simple_action_new("open-link", G_VARIANT_TYPE_STRING);
  g_signal_connect(open_link_action, "activate", G_CALLBACK(on_open_link), NULL);
}

static void start(gboolean autostarted) {
  if (started) {
    app_show_window();
    return;
  }
  started = TRUE;
  tw_log("Twincxt %s starting (ui %s)", TWINCXT_VERSION, ui_dir);
  cfg.autostart = autostart_enabled();
  if (cfg.autostart) set_autostart(TRUE);  // keep the path current (moved AppImage, deb upgrade)
  read_decoration_layout();
  g_signal_connect(gtk_settings_get_default(), "notify::gtk-decoration-layout", G_CALLBACK(on_decoration_layout), NULL);
  read_system_theme();
  char *icon = g_build_filename(ui_dir, "icons", "app.png", NULL);
  tray_init(gtk_icon_theme_has_icon(gtk_icon_theme_get_default(), "twincxt") ? "twincxt" : icon);
  g_free(icon);
  setup_web();
  if (cfg.lockEnabled && *cfg.lockHash) app_locked = TRUE;  // locked from the first frame
  g_timeout_add_seconds(15, on_lock_timer, NULL);
  update_tray_lock();
  create_window(autostarted);
  updater_init();
  updater_start();
  const char *smoke = g_getenv("TWINCXT_SMOKE");
  if (smoke && atoi(smoke) > 0) g_timeout_add_seconds((guint)atoi(smoke), smoke_next, NULL);
}

static int on_command_line(GApplication *a, GApplicationCommandLine *cl, gpointer d) {
  int argc = 0;
  char **argv = g_application_command_line_get_arguments(cl, &argc);
  gboolean autostarted = FALSE;
  for (int i = 1; i < argc; i++)
    if (!strcmp(argv[i], "--autostart")) autostarted = TRUE;
  g_strfreev(argv);
  start(autostarted && !started);
  return 0;
}

static void on_activate(GApplication *a, gpointer d) { start(FALSE); }

int main(int argc, char **argv) {
  g_set_prgname("twincxt");
  g_set_application_name("Twincxt");
  debug_on = g_getenv("TWINCXT_DEBUG") != NULL;
  for (int i = 1; i < argc; i++) {
    if (!strcmp(argv[i], "--version")) {
      printf("Twincxt %s\n", TWINCXT_VERSION);
      return 0;
    }
  }
  if (g_getenv("TWINCXT_SELFTEST")) return pin_selftest();
  init_paths();
  load_config();
  ui_dir = find_ui_dir();
  if (!ui_dir) {
    fprintf(stderr, "twincxt: can't find ui/shell.html next to the program or in /usr/share/twincxt/ui\n");
    return 1;
  }
  gboolean test_instance = g_getenv("TWINCXT_USERDATA") != NULL;
  // GNOME Shell shows a notification only for an installed APP_ID.desktop; else
  // (AppImage, tests) use the freedesktop notification server directly.
  GDesktopAppInfo *installed = g_desktop_app_info_new(APP_ID ".desktop");
  if (!installed || test_instance) g_setenv("GNOTIFICATION_BACKEND", "freedesktop", FALSE);
  g_clear_object(&installed);

  GApplicationFlags flags = G_APPLICATION_HANDLES_COMMAND_LINE;
  if (test_instance) flags |= G_APPLICATION_NON_UNIQUE;  // tests run beside a real Twincxt
  app = gtk_application_new(APP_ID, flags);
  g_signal_connect(app, "startup", G_CALLBACK(on_startup), NULL);
  g_signal_connect(app, "command-line", G_CALLBACK(on_command_line), NULL);
  g_signal_connect(app, "activate", G_CALLBACK(on_activate), NULL);
  int r = g_application_run(G_APPLICATION(app), argc, argv);
  g_object_unref(app);
  return r;
}
