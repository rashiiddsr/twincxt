// Built-in updater (same flow as desktop/updater.js and updater.go).
//
//   1. Check: the newest GitHub release is read 20 s after start, every 6 hours
//      and when "Check for updates" is pressed.
//   2. Download: with automatic checks on, a newer version downloads in the
//      background. Its SHA-256 must match the checksum GitHub lists for the file
//      (or SHA256SUMS*.txt in the release), otherwise it is deleted.
//   3. Install, only when you press "Restart to update":
//      - AppImage: the file replaces itself and starts again.
//      - .deb: installed through the system's password prompt (pkexec), or
//        opened in the software installer when that is not available.
//   Logins and settings live in the user's folders and are never touched.
//
// Minimum version: release notes may contain <!-- twincxt-min: 1.3.0 -->; older
// versions then must update (no skipping, services locked until done).
#define _GNU_SOURCE
#include "twincxt.h"

#include <errno.h>
#include <glib/gstdio.h>
#include <libsoup/soup.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define REPO "rashiiddsr/twincxt"
#define CHECK_EVERY_S (6 * 60 * 60)

static SoupSession *sess;

static struct {
  char *status, *version, *notes, *error;
  double progress;
  gboolean required;
} st;

// The release found by the last check.
static char *rel_version, *rel_notes;
static gboolean rel_required;
static char *asset_name, *asset_url, *asset_digest;
static gint64 asset_size;
static GPtrArray *sums_urls;

static gboolean busy, dl_active, dl_then_install;
static char *file, *announced;

// Download in progress.
static char *dl_want, *dl_final, *dl_partial;
static GOutputStream *dl_out;
static GInputStream *dl_in;
static GChecksum *dl_sum;
static gint64 dl_total, dl_done, dl_last;

static const char *own_version(void) {
  const char *t = g_getenv("TWINCXT_TEST_VERSION");  // tests: pretend to be older
  return t && *t ? t : TWINCXT_VERSION;
}

static void nums(const char *s, int out[4]) {
  memset(out, 0, 4 * sizeof(int));
  int n = 0;
  for (const char *p = s ? s : ""; *p && n < 4;) {
    if (g_ascii_isdigit(*p)) {
      out[n++] = atoi(p);
      while (g_ascii_isdigit(*p)) p++;
    } else p++;
  }
}

gboolean version_newer(const char *a, const char *b) {
  int pa[4], pb[4];
  nums(a, pa);
  char *bb = g_strdup(b ? b : "");
  char *dash = strchr(bb, '-');
  if (dash) *dash = 0;
  nums(bb, pb);
  g_free(bb);
  for (int i = 0; i < 4; i++)
    if (pa[i] != pb[i]) return pa[i] > pb[i];
  return FALSE;
}

static char *clean_notes(const char *body) {
  GString *out = g_string_new(NULL);
  char **lines = g_strsplit(body ? body : "", "\n", -1);
  int n = 0;
  for (int i = 0; lines[i]; i++) {
    char *l = g_strstrip(lines[i]);
    if (!*l || g_str_has_prefix(l, "<!--")) continue;
    while (*l == '#') l++;
    GString *c = g_string_new(NULL);
    for (const char *p = l; *p; p++) {
      if (*p == '`' || *p == '\r') continue;
      if (p[0] == '*' && p[1] == '*') {
        p++;
        continue;
      }
      g_string_append_c(c, *p);
    }
    char *t = g_strstrip(c->str);
    if (out->len) g_string_append_c(out, '\n');
    g_string_append(out, t);
    g_string_free(c, TRUE);
    if (++n >= 12) {
      g_string_append(out, "\n…");
      break;
    }
  }
  g_strfreev(lines);
  return g_string_free(out, FALSE);
}

static gboolean required_by(const char *body) {
  GMatchInfo *mi = NULL;
  GRegex *re = g_regex_new("<!--\\s*twincxt-min:\\s*v?([0-9][0-9.]*)\\s*-->", 0, 0, NULL);
  gboolean r = FALSE;
  if (g_regex_match(re, body ? body : "", 0, &mi)) {
    char *v = g_match_info_fetch(mi, 1);
    r = version_newer(v, own_version());
    g_free(v);
  }
  g_match_info_free(mi);
  g_regex_unref(re);
  return r;
}

// ---- state for the shell ----------------------------------------------------------------

char *updater_state_json(void) {
  GString *g = g_string_new("{\"status\":");
  json_esc(g, st.status ? st.status : "idle");
  g_string_append(g, ",\"version\":");
  json_esc(g, st.version);
  char num[G_ASCII_DTOSTR_BUF_SIZE];
  g_string_append_printf(g, ",\"progress\":%s,\"notes\":", g_ascii_formatd(num, sizeof num, "%.4f", st.progress));
  json_esc(g, st.notes);
  g_string_append(g, ",\"error\":");
  json_esc(g, st.error);
  g_string_append_printf(g, ",\"required\":%s}", st.required ? "true" : "false");
  return g_string_free(g, FALSE);
}

static void emit(void) {
  char *j = updater_state_json();
  app_update_state(j);
  g_free(j);
}

static void reset(const char *status) {
  g_free(st.status);
  g_free(st.version);
  g_free(st.notes);
  g_free(st.error);
  st.status = g_strdup(status);
  st.version = g_strdup("");
  st.notes = g_strdup("");
  st.error = g_strdup("");
  st.progress = 0;
  st.required = FALSE;
}

// The release being handled (version, notes, required) plus a status.
static void reset_release(const char *status) {
  reset(status);
  g_free(st.version);
  g_free(st.notes);
  st.version = g_strdup(rel_version ? rel_version : "");
  st.notes = g_strdup(rel_notes ? rel_notes : "");
  st.required = rel_required;
}

static void set_status(const char *status) {
  g_free(st.status);
  st.status = g_strdup(status);
}

static void set_error(const char *msg) {
  set_status("error");
  g_free(st.error);
  st.error = g_strdup(msg);
}

// ---- check ----------------------------------------------------------------------------------

static void download(gboolean then_install);

static SoupMessage *get(const char *url) {
  SoupMessage *m = soup_message_new("GET", url);
  if (m) soup_message_headers_append(soup_message_get_request_headers(m), "Accept", "application/vnd.github+json");
  return m;
}

static gboolean pick_asset(JSCValue *rel) {
  g_clear_pointer(&asset_name, g_free);
  g_clear_pointer(&asset_url, g_free);
  g_clear_pointer(&asset_digest, g_free);
  if (sums_urls) g_ptr_array_unref(sums_urls);
  sums_urls = g_ptr_array_new_with_free_func(g_free);
  const char *ai = g_getenv("APPIMAGE");
  GRegex *re = g_regex_new(ai && *ai ? "-linux-(x86_64|x64)\\.AppImage$" : "-linux-(amd64|x64)\\.deb$", G_REGEX_CASELESS, 0, NULL);
  GRegex *sums = g_regex_new("^sha256sums.*\\.txt$", G_REGEX_CASELESS, 0, NULL);
  JSCValue *assets = jobj(rel, "assets");
  int n = assets ? (int)jnum(assets, "length", 0) : 0;
  for (int i = 0; i < n; i++) {
    JSCValue *a = jsc_value_object_get_property_at_index(assets, (guint)i);
    char *name = jstr(a, "name"), *url = jstr(a, "browser_download_url");
    if (name && url) {
      if (!asset_name && g_regex_match(re, name, 0, NULL)) {
        asset_name = g_strdup(name);
        asset_url = g_strdup(url);
        asset_digest = jstr(a, "digest");
        asset_size = (gint64)jnum(a, "size", 0);
      }
      if (g_regex_match(sums, name, 0, NULL)) g_ptr_array_add(sums_urls, g_strdup(url));
    }
    g_free(name);
    g_free(url);
    g_clear_object(&a);
  }
  g_clear_object(&assets);
  g_regex_unref(re);
  g_regex_unref(sums);
  return asset_name != NULL;
}

typedef struct {
  SoupMessage *msg;
  gboolean manual;
} CheckCtx;

static void on_checked(GObject *src, GAsyncResult *res, gpointer data) {
  CheckCtx *c = data;
  GError *err = NULL;
  GBytes *b = soup_session_send_and_read_finish(SOUP_SESSION(src), res, &err);
  guint code = soup_message_get_status(c->msg);
  char *fail = NULL;
  JSCValue *rel = NULL;
  if (!b) fail = g_strdup(err ? err->message : "no answer");
  else if (code != 200) fail = g_strdup_printf("GitHub answered %u", code);
  else {
    char *txt = g_strndup(g_bytes_get_data(b, NULL), g_bytes_get_size(b));
    rel = json_parse(txt);
    g_free(txt);
    if (!rel) fail = g_strdup("GitHub's answer can't be read");
    else if (jbool(rel, "draft", FALSE) || jbool(rel, "prerelease", FALSE)) fail = g_strdup("the latest release is not a final release");
  }
  if (!fail) {
    char *tag = jstr(rel, "tag_name"), *body = jstr(rel, "body");
    const char *version = tag ? (tag[0] == 'v' ? tag + 1 : tag) : "";
    gboolean found = pick_asset(rel);
    if (!version_newer(version, own_version()) || !found) {
      tw_log("update: %s is the latest (newest release %s%s)", own_version(), version, found ? "" : ", no file for this system");
      if (c->manual) {
        reset("latest");
        emit();
      }
    } else {
      gboolean req = required_by(body);
      if (!c->manual && !req && (!cfg_auto_update() || !g_strcmp0(cfg_skipped_version(), version))) {
        tw_log("update: %s available, not downloading (automatic updates off or skipped)", version);
      } else {
        g_free(rel_version);
        g_free(rel_notes);
        rel_version = g_strdup(version);
        rel_notes = clean_notes(body);
        rel_required = req;
        tw_log("update: %s available (%s)%s", version, asset_name, req ? ", required" : "");
        reset_release("available");
        emit();
        if (req) app_lock_for_update();
        if (req || (!c->manual && cfg_auto_update())) download(FALSE);
      }
    }
    g_free(tag);
    g_free(body);
  } else {
    tw_log("update: check failed: %s", fail);
    if (c->manual) {
      reset("error");
      set_error("Can't check for updates. Check your connection and try again.");
      emit();
    }
  }
  g_free(fail);
  g_clear_object(&rel);
  g_clear_error(&err);
  if (b) g_bytes_unref(b);
  g_object_unref(c->msg);
  g_free(c);
  busy = FALSE;
}

void updater_check(gboolean manual) {
  if (busy || dl_active) return;
  busy = TRUE;
  if (manual) {
    reset("checking");
    emit();
  }
  CheckCtx *c = g_new0(CheckCtx, 1);
  c->msg = get("https://api.github.com/repos/" REPO "/releases/latest");
  c->manual = manual;
  soup_session_send_and_read_async(sess, c->msg, G_PRIORITY_DEFAULT, NULL, on_checked, c);
}

// ---- download ----------------------------------------------------------------------------

static char *file_sha256(const char *path) {
  FILE *f = fopen(path, "rb");
  if (!f) return NULL;
  GChecksum *c = g_checksum_new(G_CHECKSUM_SHA256);
  guchar buf[65536];
  size_t n;
  while ((n = fread(buf, 1, sizeof buf, f)) > 0) g_checksum_update(c, buf, (gssize)n);
  fclose(f);
  char *r = g_strdup(g_checksum_get_string(c));
  g_checksum_free(c);
  return r;
}

static gboolean is_hex64(const char *s) {
  if (!s || strlen(s) != 64) return FALSE;
  for (const char *p = s; *p; p++)
    if (!g_ascii_isxdigit(*p)) return FALSE;
  return TRUE;
}

static void dl_cleanup(void) {
  g_clear_object(&dl_in);
  if (dl_out) g_output_stream_close(dl_out, NULL, NULL);
  g_clear_object(&dl_out);
  g_clear_pointer(&dl_sum, g_checksum_free);
}

static void dl_fail(const char *why) {
  tw_log("update: download failed: %s", why);
  dl_cleanup();
  if (dl_partial) g_unlink(dl_partial);
  dl_active = FALSE;
  reset_release("error");
  char *m = g_strdup_printf("Download failed: %s", why);
  set_error(m);
  g_free(m);
  emit();
}

static void dl_ready(void) {
  g_free(file);
  file = g_strdup(dl_final);
  dl_active = FALSE;
  tw_log("update: %s downloaded and verified (%s)", rel_version, file);
  reset_release("ready");
  st.progress = 1;
  emit();
  if (dl_then_install) {
    updater_install();
    return;
  }
  if (g_strcmp0(announced, rel_version)) {
    g_free(announced);
    announced = g_strdup(rel_version);
    char *t = g_strdup_printf("Twincxt %s is ready", rel_version);
    app_notify_plain(t, "Open Settings to install it.");
    g_free(t);
  }
}

static void on_chunk(GObject *src, GAsyncResult *res, gpointer d) {
  GError *err = NULL;
  GBytes *b = g_input_stream_read_bytes_finish(G_INPUT_STREAM(src), res, &err);
  if (!b) {
    dl_fail(err->message);
    g_error_free(err);
    return;
  }
  gsize n = g_bytes_get_size(b);
  if (n == 0) {  // done
    g_bytes_unref(b);
    char *got = g_strdup(g_checksum_get_string(dl_sum));
    gboolean closed = g_output_stream_close(dl_out, NULL, &err);
    g_clear_object(&dl_out);
    if (!closed) {
      dl_fail(err->message);
      g_error_free(err);
    } else if (g_ascii_strcasecmp(got, dl_want)) {
      dl_fail("file doesn't match the release checksum");
    } else if (g_rename(dl_partial, dl_final) != 0) {
      dl_fail(g_strerror(errno));
    } else {
      dl_cleanup();
      dl_ready();
    }
    g_free(got);
    return;
  }
  gsize len = 0;
  const guchar *data = g_bytes_get_data(b, &len);
  g_checksum_update(dl_sum, data, (gssize)len);
  if (!g_output_stream_write_all(dl_out, data, len, NULL, NULL, &err)) {
    g_bytes_unref(b);
    dl_fail(err->message);
    g_error_free(err);
    return;
  }
  g_bytes_unref(b);
  dl_done += (gint64)len;
  gint64 now = g_get_monotonic_time();
  if (dl_total > 0 && now - dl_last > 200000) {
    dl_last = now;
    st.progress = (double)dl_done / (double)dl_total;
    if (st.progress > 1) st.progress = 1;
    emit();
  }
  g_input_stream_read_bytes_async(dl_in, 65536, G_PRIORITY_DEFAULT, NULL, on_chunk, NULL);
}

static void on_sent(GObject *src, GAsyncResult *res, gpointer msg) {
  GError *err = NULL;
  GInputStream *in = soup_session_send_finish(SOUP_SESSION(src), res, &err);
  guint code = soup_message_get_status(msg);
  if (!in || code != 200) {
    char *why = in ? g_strdup_printf("server answered %u", code) : g_strdup(err->message);
    dl_fail(why);
    g_free(why);
    g_clear_error(&err);
    g_clear_object(&in);
    g_object_unref(msg);
    return;
  }
  dl_total = soup_message_headers_get_content_length(soup_message_get_response_headers(msg));
  if (dl_total <= 0) dl_total = asset_size;
  g_object_unref(msg);
  dl_in = in;
  dl_done = dl_last = 0;
  dl_sum = g_checksum_new(G_CHECKSUM_SHA256);
  GFile *f = g_file_new_for_path(dl_partial);
  dl_out = G_OUTPUT_STREAM(g_file_replace(f, NULL, FALSE, G_FILE_CREATE_PRIVATE, NULL, &err));
  g_object_unref(f);
  if (!dl_out) {
    dl_fail(err->message);
    g_error_free(err);
    return;
  }
  g_input_stream_read_bytes_async(dl_in, 65536, G_PRIORITY_DEFAULT, NULL, on_chunk, NULL);
}

static void clear_dir(const char *dir) {
  GDir *d = g_dir_open(dir, 0, NULL);
  if (!d) return;
  const char *n;
  while ((n = g_dir_read_name(d))) {
    char *p = g_build_filename(dir, n, NULL);
    if (!g_file_test(p, G_FILE_TEST_IS_DIR)) g_unlink(p);
    g_free(p);
  }
  g_dir_close(d);
}

static void with_checksum(void) {
  const char *dir = app_updates_dir();
  g_mkdir_with_parents(dir, 0700);
  char *base = g_path_get_basename(asset_name);
  g_free(dl_final);
  g_free(dl_partial);
  dl_final = g_build_filename(dir, base, NULL);
  dl_partial = g_strconcat(dl_final, ".partial", NULL);
  g_free(base);
  char *have = file_sha256(dl_final);
  gboolean ok = have && !g_ascii_strcasecmp(have, dl_want);
  g_free(have);
  if (ok) {
    dl_ready();
    return;
  }
  clear_dir(dir);
  SoupMessage *m = soup_message_new("GET", asset_url);
  if (!m) {
    dl_fail("bad download address");
    return;
  }
  soup_session_send_async(sess, m, G_PRIORITY_DEFAULT, NULL, on_sent, m);
}

static void fetch_sums(guint i);

static void on_sums(GObject *src, GAsyncResult *res, gpointer idx) {
  guint i = GPOINTER_TO_UINT(idx);
  GBytes *b = soup_session_send_and_read_finish(SOUP_SESSION(src), res, NULL);
  if (b) {
    char *txt = g_strndup(g_bytes_get_data(b, NULL), g_bytes_get_size(b));
    char **lines = g_strsplit(txt, "\n", -1);
    for (int k = 0; lines[k] && !dl_want; k++) {
      char **f = g_strsplit_set(g_strstrip(lines[k]), " \t", -1);
      char *fields[3] = {NULL, NULL, NULL};
      int nf = 0;
      for (int j = 0; f[j]; j++)
        if (*f[j]) {
          if (nf < 3) fields[nf] = f[j];
          nf++;
        }
      if (nf == 2) {
        const char *name = fields[1][0] == '*' ? fields[1] + 1 : fields[1];
        if (!strcmp(name, asset_name) && is_hex64(fields[0])) dl_want = g_ascii_strdown(fields[0], -1);
      }
      g_strfreev(f);
    }
    g_strfreev(lines);
    g_free(txt);
    g_bytes_unref(b);
  }
  if (dl_want) with_checksum();
  else fetch_sums(i + 1);
}

static void fetch_sums(guint i) {
  if (!sums_urls || i >= sums_urls->len) {
    dl_fail("no checksum published for this file");
    return;
  }
  SoupMessage *m = soup_message_new("GET", g_ptr_array_index(sums_urls, i));
  if (!m) {
    fetch_sums(i + 1);
    return;
  }
  soup_session_send_and_read_async(sess, m, G_PRIORITY_DEFAULT, NULL, on_sums, GUINT_TO_POINTER(i));
  g_object_unref(m);
}

static void download(gboolean then_install) {
  if (!rel_version || !asset_url || dl_active) return;
  dl_active = TRUE;
  dl_then_install = then_install;
  reset_release("downloading");
  emit();
  g_clear_pointer(&dl_want, g_free);
  const char *d = asset_digest ? asset_digest : "";
  if (!g_ascii_strncasecmp(d, "sha256:", 7)) d += 7;
  if (is_hex64(d)) {
    dl_want = g_ascii_strdown(d, -1);
    with_checksum();
  } else {
    fetch_sums(0);
  }
}

// ---- install -------------------------------------------------------------------------------

static void detach(gpointer d) { setsid(); }

static gboolean spawn_detached(char **argv, GError **err) {
  return g_spawn_async(NULL, argv, NULL, G_SPAWN_STDOUT_TO_DEV_NULL | G_SPAWN_STDERR_TO_DEV_NULL, detach, NULL, NULL, err);
}

static gboolean quit_soon(gpointer d) {
  app_quit();
  return G_SOURCE_REMOVE;
}

static gboolean install_appimage(const char *current, GError **err) {
  char *dir = g_path_get_dirname(current), *base = g_path_get_basename(current);
  char *staged = g_strdup_printf("%s/.%s.new", dir, base);
  GFile *src = g_file_new_for_path(file), *dst = g_file_new_for_path(staged);
  gboolean ok = g_file_copy(src, dst, G_FILE_COPY_OVERWRITE, NULL, NULL, NULL, err);
  g_object_unref(src);
  g_object_unref(dst);
  if (ok && (g_chmod(staged, 0755) != 0 || g_rename(staged, current) != 0)) {  // atomic; the running copy keeps its inode
    g_set_error(err, G_FILE_ERROR, g_file_error_from_errno(errno), "Can't replace %s: %s", current, g_strerror(errno));
    g_unlink(staged);
    ok = FALSE;
  }
  if (ok) {
    // Start the new version once this one has quit (one Twincxt at a time).
    char *pid = g_strdup_printf("%d", (int)getpid());
    char *argv[] = {"/bin/sh", "-c", "while kill -0 \"$1\" 2>/dev/null; do sleep 0.3; done; exec \"$0\"", (char *)current, pid, NULL};
    ok = spawn_detached(argv, err);
    g_free(pid);
  }
  if (ok) {
    set_status("installing");
    emit();
    g_timeout_add(800, quit_soon, NULL);
  }
  g_free(staged);
  g_free(dir);
  g_free(base);
  return ok;
}

static gboolean install_deb(GError **err) {
  gboolean pk = g_file_test("/usr/bin/pkexec", G_FILE_TEST_IS_EXECUTABLE);
  char *q = g_shell_quote(file);
  char *script = g_build_filename(app_updates_dir(), "install.sh", NULL);
  char *body = pk ? g_strdup_printf("#!/bin/bash\n"
                                    "for i in $(seq 1 120); do kill -0 %d 2>/dev/null || break; sleep 0.5; done\n"
                                    "if pkexec apt-get install -y %s; then nohup twincxt >/dev/null 2>&1 & else xdg-open %s; fi\n",
                                    (int)getpid(), q, q)
                  : g_strdup_printf("#!/bin/bash\nxdg-open %s\n", q);
  gboolean ok = g_file_set_contents(script, body, -1, err) && g_chmod(script, 0755) == 0;
  if (ok) {
    char *argv[] = {"/bin/bash", script, NULL};
    ok = spawn_detached(argv, err);
  }
  if (ok) {
    set_status("installing");
    emit();
    if (pk) g_timeout_add(800, quit_soon, NULL);
  }
  g_free(body);
  g_free(script);
  g_free(q);
  return ok;
}

void updater_install(void) {
  if (dl_active) return;
  if (!file || !g_file_test(file, G_FILE_TEST_EXISTS)) {
    g_clear_pointer(&file, g_free);
    download(TRUE);
    return;
  }
  GError *err = NULL;
  const char *ai = g_getenv("APPIMAGE");
  gboolean ok = ai && *ai ? install_appimage(ai, &err) : install_deb(&err);
  if (!ok) {
    tw_log("update: install failed: %s", err ? err->message : "");
    set_error(err ? err->message : "Install failed");
    emit();
  }
  g_clear_error(&err);
}

void updater_skip(void) {
  if (!st.version || !*st.version || st.required) return;
  cfg_set_skipped_version(st.version);
  reset("idle");
  emit();
}

// ---- start ----------------------------------------------------------------------------------

static gboolean on_timer(gpointer d) {
  updater_check(FALSE);
  return G_SOURCE_CONTINUE;
}

static gboolean on_first(gpointer d) {
  updater_check(FALSE);
  return G_SOURCE_REMOVE;
}

void updater_init(void) {
  reset("idle");
  char *ua = g_strdup_printf("Twincxt-Updater/%s", TWINCXT_VERSION);
  sess = soup_session_new_with_options("user-agent", ua, "timeout", 60, NULL);
  g_free(ua);
}

void updater_start(void) {
  const char *d = g_getenv("TWINCXT_UPDATE_DELAY");
  guint first = d && *d ? (guint)atoi(d) : 20;
  g_timeout_add_seconds(first, on_first, NULL);
  g_timeout_add_seconds(CHECK_EVERY_S, on_timer, NULL);
}
