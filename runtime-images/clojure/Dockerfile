FROM eclipse-temurin:21-jdk-alpine

# bash + curl + rlwrap: required by the Clojure CLI installer
# (rlwrap powers the REPL but is harmless for non-interactive runs).
RUN apk add --no-cache bash curl rlwrap

# Clojure CLI installer. Pin the version so image rebuilds are reproducible.
ARG CLOJURE_VERSION=1.11.4.1474
RUN curl -fsSL -o /tmp/install-clojure.sh \
        "https://download.clojure.org/install/linux-install-${CLOJURE_VERSION}.sh" \
    && chmod +x /tmp/install-clojure.sh \
    && /tmp/install-clojure.sh \
    && rm /tmp/install-clojure.sh

# Sandbox user gets a REAL home directory (not under /sandbox, which is
# bind-mounted at runtime and would shadow any pre-built clojure cache there).
RUN adduser -D -u 10001 -G nobody -h /home/sandbox -s /bin/false sandbox

USER sandbox
WORKDIR /home/sandbox
ENV HOME=/home/sandbox

# Pre-fetch org.clojure/clojure + tools.deps dependencies and build the
# classpath cache (.cpcache). This is the only point where the network is
# available — the runtime sandbox has --network none, so any artifact not
# cached here will fail with "Error building classpath".
#
# We use a trivial deps.edn so the cpcache hash is stable; the user's
# `clojure main.clj` invocations with no project-level deps.edn hit this
# cache cleanly.
RUN mkdir -p /home/sandbox/.clojure \
    && echo '{:deps {org.clojure/clojure {:mvn/version "1.11.1"}}}' > /home/sandbox/.clojure/deps.edn \
    && clojure -P \
    && echo '(println "warmup")' > /tmp/warmup.clj \
    && clojure -M /tmp/warmup.clj \
    && rm /tmp/warmup.clj

# Drop back to /sandbox for normal invocation. /home/sandbox stays read-only
# at runtime (the cache only needs to be read, not written, once warmed).
WORKDIR /sandbox

RUN clojure --version && echo "Clojure runtime OK"
