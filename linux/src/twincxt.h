// Twincxt for Linux: GTK 3 + WebKitGTK (webkit2gtk-4.1), the system's own
// libraries. Shares ui/shell.html and ui/service.js with Windows and macOS.
#pragma once

#include <gtk/gtk.h>
#include <webkit2/webkit2.h>

#define APP_ID "io.github.rashiiddsr.Twincxt"

#ifndef TWINCXT_VERSION
#define TWINCXT_VERSION "0.0.0"
#endif

void tw_log(const char *fmt, ...) G_GNUC_PRINTF(1, 2);

// json.c: JSON through JavaScriptCore.
JSCValue *json_parse(const char *text);            // an object or array, or NULL
JSCValue *jprop(JSCValue *obj, const char *key);   // new ref, or NULL
char *jstr(JSCValue *obj, const char *key);        // string value or NULL
gboolean jbool(JSCValue *obj, const char *key, gboolean def);
double jnum(JSCValue *obj, const char *key, double def);
JSCValue *jobj(JSCValue *obj, const char *key);    // object/array value or NULL
void json_esc(GString *out, const char *s);         // appends "s" as a JSON string
char *json_quote(const char *s);

// main.c, used by the updater and the tray.
void app_show_window(void);
void app_select(const char *id);
void app_open_settings(void);
void app_quit(void);
void app_lock_for_update(void);
void app_update_state(const char *json);
void app_notify_plain(const char *title, const char *body);
gboolean cfg_auto_update(void);
const char *cfg_skipped_version(void);
void cfg_set_skipped_version(const char *v);
const char *app_updates_dir(void);

// updater.c
void updater_init(void);
void updater_start(void);
void updater_check(gboolean manual);
void updater_install(void);
void updater_skip(void);
char *updater_state_json(void);
gboolean version_newer(const char *a, const char *b);

// applock.c
char *pin_hash(const char *pin, const char *salt_hex);  // hex PBKDF2-HMAC-SHA256
char *pin_new_salt(void);                              // 16 random bytes, hex
gboolean pin_format_ok(const char *pin);               // ^[0-9]{4,8}$
gboolean equal_ct(const char *a, const char *b);       // constant time
char *lock_wait_text(gint64 usec);                     // "30 seconds", "5 minutes"
int pin_selftest(void);
void app_lock_now(void);

// tray.c (libayatana-appindicator3, loaded at run time when present)
gboolean tray_init(const char *icon);
gboolean tray_available(void);
void tray_update(int wa, int tg);
void tray_set_lock(gboolean show);  // the "Lock" item
