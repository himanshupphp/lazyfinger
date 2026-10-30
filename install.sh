#!/bin/sh
# Installs the latest LazyFinger release:
#   curl -fsSL https://raw.githubusercontent.com/himanshupphp/lazyfinger/main/install.sh | sh
set -eu
REPO="himanshupphp/lazyfinger"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "Unsupported OS: $os. On Windows, use Scoop or the zip from the Releases page."; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported CPU: $(uname -m)"; exit 1 ;;
esac

tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
[ -n "$tag" ] || { echo "Couldn't find the latest release."; exit 1; }
version=${tag#v}
file="lazy_${version}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading LazyFinger $version for $os/$arch..."
curl -fsSL "https://github.com/$REPO/releases/download/$tag/$file" -o "$tmp/$file"
curl -fsSL "https://github.com/$REPO/releases/download/$tag/checksums.txt" -o "$tmp/checksums.txt"

cd "$tmp"
if command -v sha256sum >/dev/null; then
  grep " $file\$" checksums.txt | sha256sum -c -
else
  grep " $file\$" checksums.txt | shasum -a 256 -c -
fi
tar -xzf "$file"

dest="${LAZY_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dest"
install -m 755 lazy "$dest/lazy"
echo "Installed lazy to $dest/lazy"

case ":$PATH:" in
  *":$dest:"*) ;;
  *) echo "Add it to your PATH:  echo 'export PATH=\"$dest:\$PATH\"' >> ~/.zshrc   (or ~/.bashrc on Linux)" ;;
esac
echo "Next: run 'lazy setup' to choose your model."
