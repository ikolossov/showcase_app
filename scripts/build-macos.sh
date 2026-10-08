#!/usr/bin/env bash
# Сборка клиента под macOS: arm64 (Apple Silicon) и x86_64 (Intel).
#
# Запускается только на Mac. Линковка под macOS требует macOS SDK из Xcode,
# а лицензия Xcode разрешает использовать его только на технике Apple, поэтому
# в Docker (как linux/windows) эта сборка не делается. В CI — раннер macos-*,
# см. .github/workflows/build.yml.
#
# Требования: Xcode Command Line Tools (xcode-select --install) и Rust (https://rustup.rs).
#
# Переменные:
#   VERSION              версия, зашиваемая в бинарник (по умолчанию 1.0.0)
#   SERVER               адрес сервера по умолчанию (попадает в QR)
#   TARGETS              список целей (по умолчанию обе архитектуры)
#   MACOS_SIGN_IDENTITY  "Developer ID Application: …" для подписи; по умолчанию ad-hoc ("-")
#   NOTARY_PROFILE       профиль `xcrun notarytool store-credentials` — если задан, бинарники
#                        отправляются на нотаризацию (нужна подпись Developer ID)
#   OUT                  каталог результатов (по умолчанию dist)
set -euo pipefail

VERSION="${VERSION:-1.0.0}"
SERVER="${SERVER:-http://localhost:8780}"
TARGETS="${TARGETS:-aarch64-apple-darwin x86_64-apple-darwin}"
SIGN_IDENTITY="${MACOS_SIGN_IDENTITY:--}"
OUT="${OUT:-dist}"
# 11.0 — минимальная версия с поддержкой Apple Silicon.
export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-11.0}"

die() { echo "build-macos: $*" >&2; exit 1; }
[[ "$(uname -s)" == Darwin ]] || die "скрипт запускается только на macOS"
xcode-select -p >/dev/null 2>&1 || die "нет Command Line Tools: выполните xcode-select --install"
command -v cargo >/dev/null || die "нет Rust: установите с https://rustup.rs"

# Без подписи arm64-бинарник не запустится. Ad-hoc подпись достаточна для
# своих машин; для раздачи пользователям — Developer ID + нотаризация.
sign() {
  if [[ "$SIGN_IDENTITY" == "-" ]]; then
    codesign --force --sign - "$1"
  else
    codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$1"
  fi
  codesign --verify "$1"
}

cd "$(dirname "$0")/.."
mkdir -p "$OUT"

built=()
for target in $TARGETS; do
  rustup target add "$target" >/dev/null
  APP_VERSION="$VERSION" SHOWCASE_DEFAULT_SERVER="$SERVER" \
    cargo build --manifest-path client/Cargo.toml --release --locked --target "$target"

  # Имя должно совпадать с тем, что клиент запрашивает у сервера обновлений:
  # std::env::consts::OS = "macos", ARCH = "aarch64" | "x86_64".
  dst="$OUT/showcase-$VERSION-macos-${target%%-*}"
  cp "client/target/$target/release/showcase" "$dst"
  sign "$dst"
  built+=("$dst")
done

# Универсальный бинарник — для ручной раздачи. Самообновление качает файл под свою архитектуру.
if [[ ${#built[@]} -eq 2 ]]; then
  universal="$OUT/showcase-$VERSION-macos-universal"
  lipo -create -output "$universal" "${built[@]}"
  sign "$universal"
  built+=("$universal")
fi

if [[ -n "${NOTARY_PROFILE:-}" ]]; then
  [[ "$SIGN_IDENTITY" != "-" ]] || die "нотаризация требует MACOS_SIGN_IDENTITY (Developer ID)"
  for f in "${built[@]}"; do
    ditto -c -k "$f" "$f.zip"
    xcrun notarytool submit "$f.zip" --keychain-profile "$NOTARY_PROFILE" --wait
    rm "$f.zip"
  done
fi

ls -l "${built[@]}"
