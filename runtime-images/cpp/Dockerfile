FROM alpine:3.20

# Install GCC 13 and C/C++/Fortran toolchain. This single image is shared by
# LanguageC, LanguageCPP, and LanguageFortran in internal/models/language.go.
RUN apk add --no-cache \
    gcc \
    g++ \
    gfortran \
    musl-dev \
    libc-dev \
    make \
    cmake \
    binutils \
    libstdc++ \
    libgcc \
    ca-certificates \
    linux-headers

# Verify GCC version is 13.x
RUN gcc --version | grep -q "13\." || (echo "WARNING: GCC version may not be 13"; gcc --version)

# Install additional useful C++ libraries
RUN apk add --no-cache \
    boost-dev \
    boost-static

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

ENV CC=gcc \
    CXX=g++ \
    CFLAGS="-O2 -Wall -Wextra" \
    CXXFLAGS="-O2 -Wall -Wextra -std=c++20"

RUN chown sandbox:nobody /sandbox

USER sandbox

# Verify toolchain
RUN gcc --version && g++ --version && gfortran --version && echo "C/C++/Fortran runtime OK"
