// App lock helpers (same as applock.go on Windows): the PIN is never stored,
// only PBKDF2-HMAC-SHA256 of it (200,000 rounds, 32-byte key, random 16-byte
// salt), both as hex in config.json. The lock itself is in main.c.
#define _GNU_SOURCE
#include "twincxt.h"

#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/random.h>
#include <unistd.h>

#define PIN_ROUNDS 200000

static guint8 *from_hex(const char *hex, gsize *len) {
  gsize n = hex ? strlen(hex) : 0;
  if (n % 2) return NULL;
  guint8 *out = g_malloc(n / 2 + 1);
  for (gsize i = 0; i < n / 2; i++) {
    int hi = g_ascii_xdigit_value(hex[2 * i]), lo = g_ascii_xdigit_value(hex[2 * i + 1]);
    if (hi < 0 || lo < 0) {
      g_free(out);
      return NULL;
    }
    out[i] = (guint8)(hi << 4 | lo);
  }
  *len = n / 2;
  return out;
}

static char *to_hex(const guint8 *b, gsize n) {
  static const char D[] = "0123456789abcdef";
  char *s = g_malloc(n * 2 + 1);
  for (gsize i = 0; i < n; i++) {
    s[2 * i] = D[b[i] >> 4];
    s[2 * i + 1] = D[b[i] & 15];
  }
  s[n * 2] = 0;
  return s;
}

// PBKDF2-HMAC-SHA256 with a 32-byte key: one block, T1 = U1 ^ U2 ^ ... ^ Uc.
char *pin_hash(const char *pin, const char *salt_hex) {
  gsize slen = 0;
  guint8 *salt = from_hex(salt_hex, &slen);
  if (!salt || !pin) {
    g_free(salt);
    return NULL;
  }
  GHmac *key = g_hmac_new(G_CHECKSUM_SHA256, (const guchar *)pin, strlen(pin));
  guint8 u[32], t[32];
  gsize ul = sizeof u;
  GHmac *h = g_hmac_copy(key);
  g_hmac_update(h, salt, (gssize)slen);
  static const guint8 one[4] = {0, 0, 0, 1};
  g_hmac_update(h, one, 4);
  g_hmac_get_digest(h, u, &ul);
  g_hmac_unref(h);
  memcpy(t, u, sizeof t);
  for (int i = 1; i < PIN_ROUNDS; i++) {
    h = g_hmac_copy(key);
    g_hmac_update(h, u, sizeof u);
    ul = sizeof u;
    g_hmac_get_digest(h, u, &ul);
    g_hmac_unref(h);
    for (int k = 0; k < 32; k++) t[k] ^= u[k];
  }
  g_hmac_unref(key);
  g_free(salt);
  char *r = to_hex(t, sizeof t);
  memset(t, 0, sizeof t);
  memset(u, 0, sizeof u);
  return r;
}

char *pin_new_salt(void) {
  guint8 b[16];
  gsize got = 0;
  while (got < sizeof b) {
    ssize_t n = getrandom(b + got, sizeof b - got, 0);
    if (n <= 0) break;
    got += (gsize)n;
  }
  if (got < sizeof b) {  // very old kernels: /dev/urandom
    int fd = open("/dev/urandom", O_RDONLY | O_CLOEXEC);
    got = 0;
    while (fd >= 0 && got < sizeof b) {
      ssize_t n = read(fd, b + got, sizeof b - got);
      if (n <= 0) break;
      got += (gsize)n;
    }
    if (fd >= 0) close(fd);
    if (got < sizeof b) return NULL;
  }
  return to_hex(b, sizeof b);
}

gboolean pin_format_ok(const char *pin) {
  size_t n = pin ? strlen(pin) : 0;
  if (n < 4 || n > 8) return FALSE;
  for (size_t i = 0; i < n; i++)
    if (pin[i] < '0' || pin[i] > '9') return FALSE;
  return TRUE;
}

gboolean equal_ct(const char *a, const char *b) {
  if (!a || !b || !*a || !*b) return FALSE;
  size_t la = strlen(a), lb = strlen(b);
  if (la != lb) return FALSE;
  unsigned char d = 0;
  for (size_t i = 0; i < la; i++) d |= (unsigned char)(a[i] ^ b[i]);
  return d == 0;
}

static const char *plural(gint64 n, const char *one, const char *many) { return n == 1 ? one : many; }

char *lock_wait_text(gint64 usec) {
  gint64 s = (usec + G_USEC_PER_SEC / 2) / G_USEC_PER_SEC;
  if (s < 1) s = 1;
  if (s >= 60 && s % 60 == 0) return g_strdup_printf("%" G_GINT64_FORMAT " %s", s / 60, plural(s / 60, "minute", "minutes"));
  return g_strdup_printf("%" G_GINT64_FORMAT " %s", s, plural(s, "second", "seconds"));
}

// TWINCXT_SELFTEST=1: checks PBKDF2 against a known vector and exits.
int pin_selftest(void) {
  const char *want = "98cd56f19733a361768b76706415776c6f3e161aef907b7e88c5ea3fe5639ab7";
  gint64 t0 = g_get_monotonic_time();
  char *got = pin_hash("1234", "00112233445566778899aabbccddeeff");
  gint64 ms = (g_get_monotonic_time() - t0) / 1000;
  gboolean ok = equal_ct(got, want);
  char *salt = pin_new_salt();
  gboolean salt_ok = salt && strlen(salt) == 32;
  char *w1 = lock_wait_text(30 * G_USEC_PER_SEC), *w2 = lock_wait_text(300 * G_USEC_PER_SEC);
  gboolean fmt_ok = pin_format_ok("1234") && pin_format_ok("12345678") && !pin_format_ok("123") && !pin_format_ok("123456789") && !pin_format_ok("12a4");
  gboolean ver_ok = version_newer("1.10.0", "1.9.9") && !version_newer("1.2.0", "1.2.0") && version_newer("1.2.1", "1.2.0-beta") && !version_newer("1.2.0", "1.3");
  printf("SELFTEST pbkdf2 %s (%s, %" G_GINT64_FORMAT " ms)\n", ok ? "ok" : "FAILED", got ? got : "null", ms);
  printf("SELFTEST salt %s (%s)\n", salt_ok ? "ok" : "FAILED", salt ? salt : "null");
  printf("SELFTEST wait text: \"%s\", \"%s\"\n", w1, w2);
  printf("SELFTEST pin format %s, version compare %s\n", fmt_ok ? "ok" : "FAILED", ver_ok ? "ok" : "FAILED");
  gboolean all = ok && salt_ok && fmt_ok && ver_ok && !strcmp(w1, "30 seconds") && !strcmp(w2, "5 minutes");
  g_free(got);
  g_free(salt);
  g_free(w1);
  g_free(w2);
  return all ? 0 : 1;
}
