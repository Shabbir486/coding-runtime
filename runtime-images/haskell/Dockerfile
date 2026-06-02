FROM haskell:9.4

# The official haskell:9.4 image is debian-based and ships multi-arch builds.
# We wrap it with the sandbox user and standardise the workdir.
RUN useradd -r -u 10001 -s /bin/false -d /sandbox -c "Sandbox execution user" sandbox \
    && mkdir -p /sandbox \
    && chown sandbox:nogroup /sandbox

WORKDIR /sandbox

USER sandbox

RUN ghc --version && runghc --version && echo "Haskell (GHC) runtime OK"
