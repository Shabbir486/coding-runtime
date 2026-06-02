FROM eclipse-temurin:21-jdk-alpine AS base

# Install dependencies for kotlinc download
RUN apk add --no-cache \
    bash \
    curl \
    unzip \
    wget

# Install Kotlin compiler
ARG KOTLIN_VERSION=2.0.21
RUN wget -q "https://github.com/JetBrains/kotlin/releases/download/v${KOTLIN_VERSION}/kotlin-compiler-${KOTLIN_VERSION}.zip" \
        -O /tmp/kotlin-compiler.zip \
    && unzip -q /tmp/kotlin-compiler.zip -d /opt \
    && mv /opt/kotlinc /opt/kotlin \
    && rm /tmp/kotlin-compiler.zip

# ---- Final stage ----
FROM eclipse-temurin:21-jdk-alpine

RUN apk add --no-cache bash

# Copy Kotlin compiler from builder
COPY --from=base /opt/kotlin /opt/kotlin

ENV PATH="/opt/kotlin/bin:${PATH}" \
    KOTLIN_HOME="/opt/kotlin" \
    # kotlinc reads JAVA_OPTS silently (unlike JAVA_TOOL_OPTIONS, which the
    # JVM echoes to stderr as "Picked up ..."). We use it to redirect jansi's
    # native-lib extraction from /tmp (mounted noexec in the sandbox, which
    # makes loading the .so fail) to /sandbox (bind-mounted, executable).
    JAVA_OPTS="-Djansi.tmpdir=/sandbox"

# JAVA_TOOL_OPTIONS is intentionally NOT set: the JVM echoes
# "Picked up JAVA_TOOL_OPTIONS: ..." to stderr at every start, polluting
# user submission output. The flags it would have carried
# (-XX:+UseContainerSupport, -XX:+UseG1GC) are JDK 10+/9+ defaults already,
# and the cgroup memory limit makes -XX:MaxRAMPercentage redundant.

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

# Verify installation
RUN kotlinc -version && echo "Kotlin runtime OK"
