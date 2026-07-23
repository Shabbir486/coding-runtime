# Web (HTML/CSS/JS) runtime — jsdom-based (Option A). Executes a combined HTML
# document's inline JS against a simulated DOM and captures console output.
FROM node:22-alpine

# Install jsdom under /runner. The runner (run.js) lives here too, so
# require('jsdom') resolves via /runner/node_modules even though user code runs
# from /sandbox.
WORKDIR /runner
RUN npm init -y >/dev/null 2>&1 \
    && npm install --no-fund --no-audit jsdom@25.0.1 qunit@2.20.0 \
    && npm cache clean --force
COPY run.js /runner/run.js

# Non-root sandbox user (matches the other runtime images).
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

ENV NODE_ENV=production \
    NODE_OPTIONS="--max-old-space-size=256" \
    NPM_CONFIG_LOGLEVEL=silent

USER sandbox

# Verify the runner + jsdom + bundled QUnit are present.
RUN node -e "require('/runner/node_modules/jsdom'); require('fs').accessSync('/runner/node_modules/qunit/qunit/qunit.js'); console.log('web (jsdom + qunit) runtime OK')"
