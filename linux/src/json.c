// JSON helpers on JavaScriptCore (part of WebKitGTK), so no extra library.
#include "twincxt.h"

static JSCContext *ctx;

static JSCContext *jctx(void) {
  if (!ctx) ctx = jsc_context_new();
  return ctx;
}

JSCValue *json_parse(const char *text) {
  if (!text) return NULL;
  JSCContext *c = jctx();
  JSCValue *v = jsc_value_new_from_json(c, text);
  if (jsc_context_get_exception(c)) {
    jsc_context_clear_exception(c);
    g_clear_object(&v);
    return NULL;
  }
  if (v && !jsc_value_is_object(v)) g_clear_object(&v);
  return v;
}

JSCValue *jprop(JSCValue *obj, const char *key) {
  if (!obj || !jsc_value_is_object(obj)) return NULL;
  JSCValue *v = jsc_value_object_get_property(obj, key);
  if (v && (jsc_value_is_undefined(v) || jsc_value_is_null(v))) g_clear_object(&v);
  return v;
}

char *jstr(JSCValue *obj, const char *key) {
  JSCValue *v = jprop(obj, key);
  char *r = NULL;
  if (v && jsc_value_is_string(v)) r = jsc_value_to_string(v);
  g_clear_object(&v);
  return r;
}

gboolean jbool(JSCValue *obj, const char *key, gboolean def) {
  JSCValue *v = jprop(obj, key);
  gboolean r = def;
  if (v && jsc_value_is_boolean(v)) r = jsc_value_to_boolean(v);
  g_clear_object(&v);
  return r;
}

double jnum(JSCValue *obj, const char *key, double def) {
  JSCValue *v = jprop(obj, key);
  double r = def;
  if (v && jsc_value_is_number(v)) r = jsc_value_to_double(v);
  g_clear_object(&v);
  return r;
}

JSCValue *jobj(JSCValue *obj, const char *key) {
  JSCValue *v = jprop(obj, key);
  if (v && !jsc_value_is_object(v)) g_clear_object(&v);
  return v;
}

void json_esc(GString *out, const char *s) {
  g_string_append_c(out, '"');
  for (const unsigned char *p = (const unsigned char *)(s ? s : ""); *p; p++) {
    switch (*p) {
      case '"': g_string_append(out, "\\\""); break;
      case '\\': g_string_append(out, "\\\\"); break;
      case '\n': g_string_append(out, "\\n"); break;
      case '\r': g_string_append(out, "\\r"); break;
      case '\t': g_string_append(out, "\\t"); break;
      default:
        if (*p < 0x20) g_string_append_printf(out, "\\u%04x", *p);
        // U+2028 / U+2029 end a line in older JavaScript.
        else if (p[0] == 0xE2 && p[1] == 0x80 && (p[2] == 0xA8 || p[2] == 0xA9)) {
          g_string_append(out, p[2] == 0xA8 ? "\\u2028" : "\\u2029");
          p += 2;
        } else g_string_append_c(out, (char)*p);
    }
  }
  g_string_append_c(out, '"');
}

char *json_quote(const char *s) {
  GString *g = g_string_new(NULL);
  json_esc(g, s);
  return g_string_free(g, FALSE);
}
