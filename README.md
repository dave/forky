I'll give you a little update on my project to turn the Go compiler into a library... I've made a lot of progress but it still seems like an impossible task to get it working. Here's some of the details as of today:

* https://github.com/dave/forky is a library assisting making bulk changes to any large codebase (at some point I'll tidy it up and release as a generic tool)  
* https://github.com/dave/forky/tree/master/forkgo is the script that uses forky to modify and make changes to the go compiler codebase
* https://github.com/dave/golib is the output it produces right now (doesn't compile yet)

It reads from `$GOPATH/src/go.googlesource.com/go` (I'll probably change this later because having a copy of the whole stdlib in my gopath is playing havoc with my editor code completion). It performs the mutations listed [here](https://github.com/dave/forky/blob/ex1/forkgo/main.go#L39), then saves the output at `$GOPATH/src/dave/golib`.

First [here](https://github.com/dave/forky/blob/ex1/forkgo/main.go#L40-L64) it deletes all the code that we don't need... I'll probably make this a bit more intelligent at some point so you just have to list so many items. 

Next [here](https://github.com/dave/forky/blob/ex1/forkgo/main.go#L65-L73) is changes all references to the package paths we're forking - this works great and operates on all string literals in the codebase - not just import statements. 

Next [here](https://github.com/dave/forky/blob/ex1/forkgo/main.go#L74-L91) we disable a bunch of tests that don't work (for various reasons). 

Next [here](https://github.com/dave/forky/blob/ex1/forkgo/main.go#L92-L103) there's a little kludge to delete a node that is relying on a go1.11 feature... I think I can remove this kludge when 1.11 drops.

If you stop at this point and save the codebase, then all the tests pass! But we really haven't done that much. Next [here](https://github.com/dave/forky/blob/ex1/forkgo/main.go#L107-L112) is the `Libify` step which is where most of the juicy stuff happens.

Libify does a bunch of things. The most obvious is that it collects all the package level vars and adds them to a `PackageSession` struct. So [this]( https://github.com/dave/golib/blob/before/src/cmd/compile/internal/x86/ssa.go#L790-L805) becomes [this](https://github.com/dave/golib/blob/after/src/cmd/compile/internal/x86/package-session.go#L20-L23) and [this](https://github.com/dave/golib/blob/after/src/cmd/compile/internal/x86/package-session.go#L38-L53).

Next it scans the bodies of all functions and methods, and works out which need access to the `PackageSession`. Any function that needs access gets a receiver added and becomes a method of the `PackageSession` type, so [this](https://github.com/dave/golib/blob/before/src/cmd/compile/internal/gc/dcl.go#L20) becomes [this](https://github.com/dave/golib/blob/after/src/cmd/compile/internal/gc/dcl.go#L12). Any reference to one of these functions (or vars) gets converted into a selector using the local `psess` variable, like [this](https://github.com/dave/golib/blob/after/src/cmd/compile/internal/gc/alg.go#L286-L287).

Any method of another type that needs access to `PackageSession` has it added as a parameter, like [this](https://github.com/dave/golib/blob/after/src/cmd/compile/internal/gc/bimport.go#L202). This needs a re-think, because changing the signature of methods means we stop satisfying interfaces (more about this later). 

Calling a public function or accessing a public variable in another package is accomplished by keeping the `PackageSession` for all imported packages in the local `PackageSession`, like [this](https://github.com/dave/golib/blob/after/src/cmd/compile/internal/gc/package-session.go#L17-L26). Any time they are accessed, the are wired up like [here](https://github.com/dave/golib/blob/after/src/cmd/internal/obj/x86/asm6.go#L1947) and [here](https://github.com/dave/golib/blob/after/src/cmd/compile/main.go#L15).

There's plenty more work needed before this will even compile. As I mentioned, we can't rely on injecting the `PackageSession` into methods because we break interfaces. See [this issue](https://github.com/dave/forky/issues/2) for more details.

One big optimization that I'm currently missing... Most of these package-level variables are never modified after initialisation. If we're sure they're never modified after initialisation, they can stay as package-level and don't need to be stored in the PackageSession. This is something that could possible be detected using the ssa/pointer analysis packages... Not something I've used before so would love some help. I posted a stackoverflow question about this yesterday [here](https://stackoverflow.com/questions/51393995/go-static-analysis-find-read-only-package-level-vars). If you have any input, discuss in [this issue](https://github.com/dave/forky/issues/3).
