#!/usr/bin/env bash
set -eo pipefail

declare -r RUST_VERSION=1.52.1
# For security, we specify a particular rustup version and a SHA256 hash, computed
# ourselves and hardcoded here. When updating `RUSTUP_VERSION`:
#   1. Download the new rustup script from https://github.com/rust-lang/rustup/releases.
#   2. Audit the script and changes to it. You might want to grep for strange URLs...
#   3. Update `RUSTUP_SHA` with the result of running `sha256sum rustup-init.sh`.
declare -r RUSTUP_VERSION=1.24.2
declare -r RUSTUP_SHA=40229562d4fa60e102646644e473575bae22ff56c3a706898a47d7241c9c031e
declare -r RUSTUP=${HOME}/.cargo/bin/rustup

function install_linux_target () {
    case $(dpkg --print-architecture) in
        amd64)
            ${RUSTUP} target add x86_64-unknown-linux-musl
            ;;
        arm64)
            ${RUSTUP} target add aarch64-unknown-linux-musl
            ;;
        *)
            >&2 echo Error: unknown arch $(dpkg --print-architecture)
            exit 1
            ;;
    esac
}

function install_windows_target () {
    ${RUSTUP} target add x86_64-pc-windows-gnu

    # Cargo's built-in support for fetching dependencies from GitHub requires
    # an ssh agent to be set up, which doesn't work on Circle's Windows executors.
    # See https://github.com/rust-lang/cargo/issues/1851#issuecomment-450130685
    cat <<EOF >> ~/.cargo/config
[net]
git-fetch-with-cli = true
EOF
}

function main () {
    # Download rustup script
    curl --proto '=https' --tlsv1.2 -sSf \
        https://raw.githubusercontent.com/rust-lang/rustup/${RUSTUP_VERSION}/rustup-init.sh -O
    # Verify checksum of rustup script. Exit with error if check fails.
    echo "${RUSTUP_SHA} rustup-init.sh" | sha256sum --check -- || { echo "Checksum problem!"; exit 1; }
    sh rustup-init.sh --default-toolchain ${RUST_VERSION} -y
    ${RUSTUP} --version

    case $(uname) in
        Linux)
            install_linux_target
            ;;
        Darwin)
            ;;
        MSYS_NT*)
            install_windows_target
            ;;
        *)
            >&2 echo Error: unknown OS $(uname)
            exit 1
            ;;
    esac

    ${RUSTUP} target list --installed
}

main ${@}
