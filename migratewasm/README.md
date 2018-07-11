## work in progress

If you'd like to give it a go, you'll need to do something like this:

```
mkdir $GOPATH/src/go.googlesource.com && cd $GOPATH/src/go.googlesource.com && git clone https://go.googlesource.com/go

go get -u github.com/dave/services/migraty/migratewasm && migratewasm

cd $GOPATH/src/github.com/dave/wasmgo && go test ./...
```