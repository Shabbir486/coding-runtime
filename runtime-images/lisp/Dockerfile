FROM alpine:3.20

# Alpine community ships sbcl (Steel Bank Common Lisp) with arm64 + amd64
# builds — replaces the fukamachi/sbcl image which is amd64-only.
RUN apk add --no-cache sbcl

RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN sbcl --version && echo "Common Lisp (SBCL) runtime OK"
