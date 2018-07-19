## work in progress

If you'd like to give it a go, you'll need to do something like this:

```
mkdir $GOPATH/src/go.googlesource.com && cd $GOPATH/src/go.googlesource.com && git clone https://go.googlesource.com/go

go get -u github.com/dave/forky/forkgo && forkgo
```

Code will appear in `$GOPATH/src/dave/golib`