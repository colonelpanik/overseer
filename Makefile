.PHONY: build test vet fmt clean

build:
	go build -o overseer ./cmd/overseer

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f overseer
