# acp-kit Makefile, modeled after sibling poe-acp / slack-acp.
#
# Library module: no cmd/, no cross-compile, no notices, no deploy.
# `make` runs fmt + tidy, then in parallel: vet, unit coverage (100% gate),
# and the e2e suite.
.DEFAULT_GOAL := all

BINDIR := bin

GO_LICENSES := go run github.com/google/go-licenses@v1.6.0
COVGATE     := go tool covgate

# ---------------------------------------------------------------------------
# Quiet step helper: $(call RUN,label,command). V=1 for verbose output.
# ---------------------------------------------------------------------------
ifdef V
  define RUN
	@printf "  %-28s\n" "$(1)"
	$(2)
  endef
else
  define RUN
	@_log=$$(mktemp) && ( $(2) ) > $$_log 2>&1 \
		&& { printf "  %-28s ✓\n" "$(1)"; rm -f $$_log; } \
		|| { printf "  %-28s ✗\n" "$(1)"; cat $$_log; rm -f $$_log; exit 1; }
  endef
endif

.PHONY: all _parallel fmt tidy vet test test-race test-race-cover e2e \
        test-cover open-coverage clean check-licenses

# ---------------------------------------------------------------------------
# Top-level: `make` = fmt + tidy serially, then everything else in parallel.
# ---------------------------------------------------------------------------
all: fmt tidy
	@$(MAKE) -j --no-print-directory _parallel

_parallel: vet test-race-cover e2e check-licenses

fmt:
	@gofmt -s -w .

tidy:
	@go mod tidy

vet:
	$(call RUN,vet,go vet ./...)

$(BINDIR):
	@mkdir -p $@

# ---------------------------------------------------------------------------
# Unit tests + 100% coverage gate.
# ---------------------------------------------------------------------------
test:
	go test ./...

test-race:
	$(call RUN,test (race),go test -race -shuffle=on ./...)

# Full unit run: race + shuffle + 100% coverage gate via covgate.
# Paths in .covignore are stripped before the gate fires.
test-race-cover: | $(BINDIR)
	$(call RUN,test (race+cover),\
		go test -race -shuffle=on -covermode=atomic -coverprofile=$(BINDIR)/coverage.tmp.out ./... \
		&& $(COVGATE) -profile=$(BINDIR)/coverage.tmp.out -out=$(BINDIR)/coverage.out -ignore=.covignore -min=100)

test-cover: | $(BINDIR)
	go test -coverprofile=$(BINDIR)/coverage.out ./...
	go tool cover -func=$(BINDIR)/coverage.out

open-coverage:
	go tool cover -html=$(BINDIR)/coverage.out

# ---------------------------------------------------------------------------
# End-to-end suite.
#
# E2e tests live under -tags e2e. They spawn a real ACP child or wire an
# in-process fake over io.Pipe and exercise the full client/state path.
# Coverage is NOT counted by the unit-test gate.
# ---------------------------------------------------------------------------
e2e:
	$(call RUN,e2e,go test -tags e2e -count=1 -race ./...)

# ---------------------------------------------------------------------------
# Licenses.
# ---------------------------------------------------------------------------
check-licenses:
	$(call RUN,check licenses,$(GO_LICENSES) check ./... --disallowed_types=forbidden,restricted 2>/dev/null)

clean:
	rm -rf $(BINDIR)
