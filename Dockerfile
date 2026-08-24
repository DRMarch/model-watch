ARG APP_UID=1000
ARG APP_GID=1000
ARG APP_USER=modelwatch

FROM golang:1.27-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

COPY go.mod ./
COPY main.go ./

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/model-watch .

ARG APP_UID
ARG APP_GID
ARG APP_USER
RUN mkdir -p /out/data && \
    chown ${APP_UID}:${APP_GID} /out/data && \
    echo "${APP_USER}:x:${APP_UID}:${APP_GID}:model-watch:/data:/sbin/nologin" > /out/passwd && \
    echo "${APP_USER}:x:${APP_GID}:" > /out/group

FROM scratch

ARG APP_UID
ARG APP_GID

COPY --from=build /out/model-watch /usr/local/bin/model-watch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group /etc/group
COPY --chown=${APP_UID}:${APP_GID} --from=build /out/data /data

WORKDIR /data

ENV SNAPSHOT_PATH=/data/models-snapshot.json

USER ${APP_UID}:${APP_GID}

ENTRYPOINT ["/usr/local/bin/model-watch"]
