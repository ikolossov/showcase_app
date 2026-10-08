# Кросс-сборка клиента: linux-x86_64 (glibc 2.36+) и windows-x86_64 (mingw).
# docker build --output type=local,dest=../dist --build-arg VERSION=1.0.0 .

FROM rust:1.99.0-slim-bookworm AS toolchain
RUN apt-get update && apt-get install -y --no-install-recommends gcc-mingw-w64-x86-64 \
    && rm -rf /var/lib/apt/lists/* \
    && rustup target add x86_64-pc-windows-gnu
WORKDIR /src

FROM toolchain AS build
ARG VERSION=1.0.0
ARG DEFAULT_SERVER=http://localhost:8080
ARG CARGO_FLAGS=""
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/target \
    APP_VERSION=$VERSION SHOWCASE_DEFAULT_SERVER=$DEFAULT_SERVER \
    cargo build --release --locked $CARGO_FLAGS \
        --target x86_64-unknown-linux-gnu --target x86_64-pc-windows-gnu \
 && mkdir /out \
 && cp target/x86_64-unknown-linux-gnu/release/showcase  /out/showcase-$VERSION-linux-x86_64 \
 && cp target/x86_64-pc-windows-gnu/release/showcase.exe /out/showcase-$VERSION-windows-x86_64.exe

# Необязательное сжатие UPX (--build-arg COMPRESS=1): примерно в 2.5 раза меньше,
# но распаковка при старте и ложные срабатывания антивирусов на Windows.
FROM alpine:3.22 AS pack
ADD --checksum=sha256:402162aad30af47e60dbd767fb2e64ca394ace9727ba1f40283641f1d1b91657 \
    https://github.com/upx/upx/releases/download/v5.2.1/upx-5.2.1-amd64_linux.tar.xz /tmp/upx.tar.xz
RUN tar -xJf /tmp/upx.tar.xz -C /tmp && mv /tmp/upx-*/upx /usr/local/bin/
COPY --from=build /out/ /out/
ARG COMPRESS=0
RUN if [ "$COMPRESS" = 1 ]; then upx -q --best --lzma /out/*; fi

FROM scratch
COPY --from=pack /out/ /
