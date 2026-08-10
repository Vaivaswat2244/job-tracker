BIN := bin/tracker

.PHONY: build test install-timers clean

build:
	go build -o $(BIN) ./cmd/tracker

test:
	go test ./...

# Timers run the compiled binary, so build before (re)installing them.
install-timers: build
	./systemd/install.sh

clean:
	rm -f $(BIN)
