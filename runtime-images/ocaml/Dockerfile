FROM alpine:3.20

# Alpine community ships ocaml + ocamlfind with native arm64 support, avoiding
# the ocaml/opam:alpine image (amd64-only on older tags). The bundled compiler
# version is 5.1.x rather than 5.4 — bump the Alpine base when 5.4 lands.
#
# gcc + musl-dev + libc-dev: ocamlopt shells out to the C linker to produce
# native binaries; without them you get "gcc: error: ..crt1.o: No such file"
# or similar from the assemble/link stage.
RUN apk add --no-cache \
    ocaml \
    ocaml-compiler-libs \
    ocaml-runtime \
    gcc \
    musl-dev \
    libc-dev

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN ocaml -version && ocamlopt -version && echo "OCaml runtime OK"
