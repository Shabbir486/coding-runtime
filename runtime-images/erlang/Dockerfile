FROM erlang:26-alpine

# Upstream erlang:26-alpine is multi-arch (arm64 + amd64).
RUN adduser -S -u 10001 -G nobody -H -s /bin/false -g "Sandbox execution user" sandbox

WORKDIR /sandbox
RUN chown sandbox:nobody /sandbox

USER sandbox

RUN erl -eval 'erlang:display(erlang:system_info(otp_release)), halt().' -noshell && echo "Erlang runtime OK"
