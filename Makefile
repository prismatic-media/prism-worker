BINARY     := prism-worker
CMD        := main.go

.PHONY: all build run test lint clean

all: build

build:
	go build -o $(BINARY) $(CMD)

run: build
	./$(BINARY)

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -f $(BINARY)
