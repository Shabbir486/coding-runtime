FROM eclipse-temurin:21-jdk-alpine

# Install minimal build tools
RUN apk add --no-cache \
    bash \
    curl

# Create non-root sandbox user
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox

# JAVA_TOOL_OPTIONS is intentionally NOT set: the JVM echoes
# "Picked up JAVA_TOOL_OPTIONS: ..." to stderr at every start, polluting
# user submission output. The flags it would have carried
# (-XX:+UseContainerSupport, -XX:+UseG1GC) are JDK 10+/9+ defaults; the
# cgroup memory limit makes -XX:MaxRAMPercentage redundant; and
# -Djava.security.manager=disallow is the default on JDK 21+.
ENV JAVA_HOME=/opt/java/openjdk

RUN chown sandbox:nobody /sandbox

USER sandbox

# Smoke test
RUN java -version 2>&1 | grep -q "21" && echo "Java 21 runtime OK"
