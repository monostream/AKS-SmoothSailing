FROM golang:1.20-alpine3.18 as builder

WORKDIR /src

COPY . ./

RUN go mod download
RUN go build


FROM mcr.microsoft.com/azure-cli

ENV KUBECTL_VERSION="v1.25.10"
RUN curl -fsSL "https://storage.googleapis.com/kubernetes-release/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" -o /usr/local/bin/kubectl && chmod +x /usr/local/bin/kubectl

WORKDIR /app

COPY --from=builder --chown=nobody:nobody /src/pv-migration .

RUN addgroup -g 1000 migrator \
    && adduser -D -u 1000 -G migrator -s /bin/ash migrator \
    && chown migrator:migrator /app

USER migrator

ENV USER migrator