#!/usr/bin/env bash
set -eo pipefail

function build_linux () {
    TAGS=osusergo,netgo,static_build,assets
    if [[ $(go env GOARCH) != amd64 ]]; then
        TAGS="$TAGS,noasm"
    fi

    local -r commit=$(git rev-parse --short HEAD)
    local -r build_date=$(date -u +'%Y-%m-%dT%H:%M:%SZ')
    local -r extld="-fno-PIC -static -Wl,-z,stack-size=8388608"
    PKG_CONFIG=$(which pkg-config) CC=musl-gcc go build \
        -tags "$TAGS" \
        -buildmode pie \
        -ldflags "-s -w -X main.version=dev -X main.commit=${commit} -X main.date=${build_date} -extldflags '${extld}'" \
        -o "${1}/" \
        ./cmd/influxd/
}

function build_mac () {
    local -r commit=$(git rev-parse --short HEAD)
    local -r build_date=$(date -u +'%Y-%m-%dT%H:%M:%SZ')
    PKG_CONFIG=$(which pkg-config) go build \
        -tags assets \
        -buildmode pie \
        -ldflags "-s -w -X main.version=dev -X main.commit=${commit} -X main.date=${build_date}" \
        -o "${1}/" \
        ./cmd/influxd/
}

function build_windows () {

}

function main () {
    if [[ $# != 1 ]]; then
        >&2 echo Usage: $0 '<output-dir>'
        exit 1
    fi
    local -r out_dir=$1

    rm -rf "$out_dir"
    mkdir -p "$out_dir"
    case $(go env GOOS) in
        linux)
            build_linux "$out_dir"
            ;;
        darwin)
            build_mac "$out_dir"
            ;;
        windows)
            build_windows "$out_dir"
            ;;
        *)
            >&2 echo Error: unknown OS $(go env GOOS)
            exit 1
            ;;
    esac

    "${out_dir}/influxd" version
}

main ${@}
