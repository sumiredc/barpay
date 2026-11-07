# The image deploy/ runs: BarPay, the development tools (barpay-dev) and the sample app, each
# started by its container's command. It is for running the sample app against the PayPal sandbox,
# not a production build of BarPay.
#
# Build from the repository root, which holds both Go modules (BarPay's and examples/mobile-order).

FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
ARG TARGETOS
ARG TARGETARCH
# SQLite is the pure Go modernc.org/sqlite, so no C toolchain is needed. GOAMD64 stays at its
# default v1: the cluster's nodes have no AVX.
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY examples/mobile-order/go.mod examples/mobile-order/go.sum examples/mobile-order/
RUN go mod download && cd examples/mobile-order && go mod download
COPY . .
# The sample app's templates and static files are embedded, so the binaries are all the image needs.
RUN go build -trimpath -o /out/barpay ./cmd/barpay \
 && go build -trimpath -o /out/barpay-dev ./cmd/barpay-dev \
 && cd examples/mobile-order && go build -trimpath -o /out/mobile-order .

FROM scratch
# BarPay calls PayPal over https.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/ /usr/local/bin/
# A numeric user, so Kubernetes can check runAsNonRoot without a passwd file.
USER 65532:65532
