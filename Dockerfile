ARG BASE="alpine:3.12"
FROM ${BASE}

ARG TARGETOS
ARG TARGETARCH

USER root

COPY .build/${TARGETOS}-${TARGETARCH}/prometheus-amazon-workspaces-sd /bin/prometheus-amazon-workspaces-sd

RUN adduser -u 888 -D prometheus && \
    mkdir /home/prometheus/.aws && \
    mkdir /prometheus-amazon-workspaces-sd && \
    chown nobody:nogroup /prometheus-amazon-workspaces-sd

USER       nobody
EXPOSE     9888
VOLUME     ["/prometheus-amazon-workspaces-sd"]
WORKDIR    /prometheus-amazon-workspaces-sd
ENTRYPOINT ["/bin/prometheus-amazon-workspaces-sd"]
CMD        ["--output.file=/prometheus-amazon-workspaces-sd/workspaces_sd.json"]
