# beefcat — Lightweb's reference BEEF-carrier client (lightwebinc/beefcat),
# built from pinned source and used as an independent conformance client of
# the header-shim's /v1/tip + /v1/root contract. The Go implementation is a
# second implementation of the carrier pattern our TS code vendored; here it
# verifies through OUR shim, so drift in the shim's contract shows up as the
# reference verifier failing.
FROM golang:1.27-alpine AS build
ARG BEEFCAT_REF=v0.1.1
RUN apk add --no-cache git
# go.sum in the repo pins the module tree; the ref pins the code
RUN git clone --depth 1 --branch "${BEEFCAT_REF}" https://github.com/lightwebinc/beefcat /src
WORKDIR /src
RUN go build -o /out/ ./cmd/...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/beefcat /out/devchain /usr/local/bin/
COPY smoke.sh /usr/local/bin/beefcat-smoke
RUN chmod +x /usr/local/bin/beefcat-smoke
ENTRYPOINT ["beefcat-smoke"]
