// Tray icon through libayatana-appindicator3 (or the older libappindicator3),
// loaded at run time: without it, or without a tray on the desktop (GNOME
// without the AppIndicator extension), "Keep in tray" minimizes instead.
#include "twincxt.h"

#include <dlfcn.h>

typedef void *(*new_fn)(const char *id, const char *icon, int category);
typedef void (*int_fn)(void *ind, int v);
typedef void (*menu_fn)(void *ind, GtkMenu *menu);
typedef void (*str_fn)(void *ind, const char *s);
typedef void (*widget_fn)(void *ind, GtkWidget *w);

static void *ind;
static str_fn set_title;
static gboolean host;  // a StatusNotifierWatcher (the tray) is running
static GtkWidget *item_wa, *item_tg;

static void on_open(GtkMenuItem *i, gpointer d) { app_show_window(); }
static void on_wa(GtkMenuItem *i, gpointer d) { app_select("wa"); }
static void on_tg(GtkMenuItem *i, gpointer d) { app_select("tg"); }
static void on_settings(GtkMenuItem *i, gpointer d) { app_open_settings(); }
static void on_quit(GtkMenuItem *i, gpointer d) { app_quit(); }

static GtkWidget *item(GtkWidget *menu, const char *label, GCallback cb) {
  GtkWidget *it = label ? gtk_menu_item_new_with_label(label) : gtk_separator_menu_item_new();
  if (cb) g_signal_connect(it, "activate", cb, NULL);
  gtk_menu_shell_append(GTK_MENU_SHELL(menu), it);
  return it;
}

static void on_appeared(GDBusConnection *c, const char *name, const char *owner, gpointer d) {
  host = TRUE;
  tw_log("tray: available");
}

static void on_vanished(GDBusConnection *c, const char *name, gpointer d) {
  host = FALSE;
  tw_log("tray: no tray on this desktop");
}

gboolean tray_init(const char *icon) {
  if (g_getenv("TWINCXT_NO_TRAY")) return FALSE;
  void *lib = dlopen("libayatana-appindicator3.so.1", RTLD_LAZY | RTLD_LOCAL);
  if (!lib) lib = dlopen("libappindicator3.so.1", RTLD_LAZY | RTLD_LOCAL);
  if (!lib) {
    tw_log("tray: libayatana-appindicator3 is not installed");
    return FALSE;
  }
  new_fn mk = (new_fn)dlsym(lib, "app_indicator_new");
  int_fn set_status = (int_fn)dlsym(lib, "app_indicator_set_status");
  menu_fn set_menu = (menu_fn)dlsym(lib, "app_indicator_set_menu");
  widget_fn set_secondary = (widget_fn)dlsym(lib, "app_indicator_set_secondary_activate_target");
  set_title = (str_fn)dlsym(lib, "app_indicator_set_title");
  if (!mk || !set_status || !set_menu) {
    tw_log("tray: unexpected appindicator library");
    return FALSE;
  }
  ind = mk("twincxt", icon, 0 /* APP_INDICATOR_CATEGORY_APPLICATION_STATUS */);
  if (!ind) return FALSE;
  GtkWidget *menu = gtk_menu_new();
  GtkWidget *open = item(menu, "Open Twincxt", G_CALLBACK(on_open));
  item(menu, NULL, NULL);
  item_wa = item(menu, "WhatsApp", G_CALLBACK(on_wa));
  item_tg = item(menu, "Telegram", G_CALLBACK(on_tg));
  item(menu, NULL, NULL);
  item(menu, "Settings", G_CALLBACK(on_settings));
  item(menu, "Quit", G_CALLBACK(on_quit));
  gtk_widget_show_all(menu);
  set_menu(ind, GTK_MENU(menu));
  if (set_secondary) set_secondary(ind, open);  // middle click opens the window
  if (set_title) set_title(ind, "Twincxt");
  set_status(ind, 1 /* APP_INDICATOR_STATUS_ACTIVE */);
  g_bus_watch_name(G_BUS_TYPE_SESSION, "org.kde.StatusNotifierWatcher", G_BUS_NAME_WATCHER_FLAGS_NONE, on_appeared, on_vanished, NULL, NULL);
  return TRUE;
}

gboolean tray_available(void) { return ind && host; }

void tray_update(int wa, int tg) {
  if (!ind) return;
  char *a = wa > 0 ? g_strdup_printf("WhatsApp  (%d)", wa) : g_strdup("WhatsApp");
  char *b = tg > 0 ? g_strdup_printf("Telegram  (%d)", tg) : g_strdup("Telegram");
  gtk_menu_item_set_label(GTK_MENU_ITEM(item_wa), a);
  gtk_menu_item_set_label(GTK_MENU_ITEM(item_tg), b);
  if (set_title) {
    char *t = wa + tg > 0 ? g_strdup_printf("Twincxt · %d unread", wa + tg) : g_strdup("Twincxt");
    set_title(ind, t);
    g_free(t);
  }
  g_free(a);
  g_free(b);
}
