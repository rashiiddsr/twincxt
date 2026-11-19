# Twincxt for Linux. Needs: gcc, pkg-config, libgtk-3-dev, libwebkit2gtk-4.1-dev,
# libsoup-3.0-dev. `make` builds build/twincxt; `make run` starts it from here
# (it finds ../ui by itself); ./package.sh makes the .deb and the AppImage.
VERSION ?= $(shell cat ../VERSION 2>/dev/null || echo 0.0.0)
PKGS = gtk+-3.0 webkit2gtk-4.1 libsoup-3.0 gio-unix-2.0
CC ?= gcc
CFLAGS ?= -O2 -g
CFLAGS += -std=gnu11 -Wall -Wextra -Wno-unused-parameter -Wno-deprecated-declarations \
	-Wno-missing-field-initializers -fstack-protector-strong -D_FORTIFY_SOURCE=2 \
	$(shell pkg-config --cflags $(PKGS)) -DTWINCXT_VERSION='"$(VERSION)"'
LDFLAGS += -Wl,--as-needed -Wl,-z,relro -Wl,-z,now
LDLIBS += $(shell pkg-config --libs $(PKGS)) -ldl

SRC = src/main.c src/updater.c src/tray.c src/json.c src/applock.c

build/twincxt: $(SRC) src/twincxt.h Makefile ../VERSION
	@mkdir -p build
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ $(SRC) $(LDLIBS)

run: build/twincxt
	TWINCXT_DEBUG=1 ./build/twincxt

clean:
	rm -rf build

.PHONY: run clean
