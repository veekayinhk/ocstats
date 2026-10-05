# ocstats — build & install
#
#   make install   symlink the Python CLI into ~/.local/bin (zero deps)
#   make build     compile the Go binary (needs Go toolchain + network once)
#   make test      cross-check all three implementations agree on totals

PREFIX ?= $(HOME)/.local/bin

.PHONY: all install build test check-go clean

all: install

install:
	@mkdir -p $(PREFIX)
	@ln -sf "$(CURDIR)/ocstats" $(PREFIX)/ocstats
	@ln -sf "$(CURDIR)/ocstats.sh" $(PREFIX)/ocstats.sh
	@echo "installed: $(PREFIX)/ocstats, $(PREFIX)/ocstats.sh"

build: check-go
	cd ocstats-go && go mod tidy && go build -o ../ocstats-bin .
	@echo "built: $(CURDIR)/ocstats-bin"

check-go:
	@command -v go >/dev/null 2>&1 || { \
	  echo "error: Go toolchain not found — install go >= 1.22 to build ocstats-bin"; \
	  echo "       (the Python and Bash implementations need no build)"; exit 1; }

test: install
	./tests/compare.sh

clean:
	rm -f ocstats-bin
