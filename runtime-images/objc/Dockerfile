FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# gobjc: Objective-C front-end for GCC
# gnustep-base-dev: Foundation framework headers and libs
# gnustep-base-runtime: NSConstantString and other runtime objects
RUN apt-get update && apt-get install -y --no-install-recommends \
    gobjc \
    libgnustep-base-dev \
    gnustep-base-runtime \
    gnustep-common \
    && rm -rf /var/lib/apt/lists/*

# Compile helper: wraps the user's main() in an NSAutoreleasePool so GNUstep
# Foundation calls don't print "autorelease called without pool" warnings
# (Apple's Cocoa silently handles this; GNUstep does not). GCC's gobjc speaks
# Objective-C 1.0 only — no @autoreleasepool block syntax — hence the classic
# [[NSAutoreleasePool alloc] init] / [pool release] pattern.
#
# The script renames the user's `int main(` to `int __user_main(` via sed, then
# appends a real main that brackets the call in the pool. The empty-prototype
# `extern int __user_main();` declaration lets us call it with (argc, argv)
# regardless of whether the user declared `int main()` or
# `int main(int, char**)`.
RUN printf '%s\n' \
    '#!/bin/sh' \
    'set -e' \
    'SRC="${1:-/sandbox/main.m}"' \
    'OUT="${2:-/sandbox/main}"' \
    'WRAPPED="$(mktemp /tmp/objc_wrapped.XXXXXX.m)"' \
    'cp "$SRC" "$WRAPPED"' \
    'sed -i -E "s/\\bint[[:space:]]+main[[:space:]]*\\(/int __user_main(/" "$WRAPPED"' \
    'printf "\\n" >> "$WRAPPED"' \
    'cat >> "$WRAPPED" <<EOF' \
    '#import <Foundation/Foundation.h>' \
    'extern int __user_main();' \
    'int main(int argc, char **argv) {' \
    '    NSAutoreleasePool *__sandbox_pool = [[NSAutoreleasePool alloc] init];' \
    '    int rc = __user_main(argc, argv);' \
    '    [__sandbox_pool release];' \
    '    return rc;' \
    '}' \
    'EOF' \
    'exec gcc -std=gnu99 -I/usr/include/GNUstep -fconstant-string-class=NSConstantString "$WRAPPED" -o "$OUT" -lgnustep-base -lobjc -lpthread' \
    > /usr/local/bin/sandbox-objc-compile \
    && chmod 0555 /usr/local/bin/sandbox-objc-compile

RUN adduser --system --uid 10001 --ingroup nogroup --no-create-home \
    --shell /bin/false --gecos "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nogroup /sandbox

USER sandbox

# Smoke-test: exercise the wrapper script and confirm autorelease warnings
# are gone. NSLog itself writes its prefix line ("YYYY-MM-DD HH:MM:SS prog[pid:tid]
# OK") to stderr by design, so we filter only the autorelease-without-pool
# warnings and require none of them survive.
RUN echo '#import <Foundation/Foundation.h>' > /tmp/test.m \
    && echo 'int main(){NSLog(@"OK"); return 0;}' >> /tmp/test.m \
    && sandbox-objc-compile /tmp/test.m /tmp/test_objc \
    && /tmp/test_objc 2>/tmp/err >/dev/null \
    && { ! grep -q 'autorelease called without pool' /tmp/err; } \
    && echo "Objective-C (GNUstep) runtime OK"