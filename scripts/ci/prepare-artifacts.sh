#!/usr/bin/env bash
set -eo pipefail

declare -r CI_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"
declare -r ROOT_DIR="$(dirname "$(dirname "$CI_SCRIPT_DIR")")"
declare -r DIST_DIR="$ROOT_DIR/dist"

function main () {
    local -r out_dir=$1 version=dev # TODO: Pass in version
    if [ ! -d "$out_dir" ]; then
        >&2 echo Error: "'$out_dir'" is not a directory
        exit 1
    fi
    local -r sha_file=${out_dir}/influxdb2-${version}.sha256

    # Linux AMD64
    (
        set -eo pipefail
        cd "$DIST_DIR/influxd_linux_amd64"

        # Create archive
        archive_dir=influxdb2-${version}-linux-amd64
        archive=${archive_dir}.tar.gz

        mkdir ${archive_dir}
        cp ${ROOT_DIR}/LICENSE ${ROOT_DIR}/README.md influxd ${archive_dir}/
        tar czf ${archive} ${archive_dir}

        # Hash the archive, deb, and rpm.
        deb=influxdb2-${version}-amd64.deb
        rpm=influxdb2-${version}.x86_64.rpm
        sha256sum ${archive} ${deb} ${rpm} >> ${sha_file}

        # Copy the archive, deb, and rpm to the output dir.
        cp ${archive} ${deb} ${rpm} ${out_dir}/
    )

    # Linux ARM64
    (
        set -eo pipefail
        cd "$DIST_DIR/influxd_linux_arm64"

        # Create archive
        archive_dir=influxdb2-${version}-linux-arm64
        archive=${archive_dir}.tar.gz

        mkdir ${archive_dir}
        cp ${ROOT_DIR}/LICENSE ${ROOT_DIR}/README.md influxd ${archive_dir}/
        tar czf ${archive} ${archive_dir}

        # Hash the archive, deb, and rpm.
        deb=influxdb2-${version}-arm64.deb
        rpm=influxdb2-${version}.aarch64.rpm
        sha256sum ${archive} ${deb} ${rpm} >> ${sha_file}

        # Copy the archive, deb, and rpm to the output dir.
        cp ${archive} ${deb} ${rpm} ${out_dir}/
    )

    # Mac AMD64
    (
        set -eo pipefail
        cd "$DIST_DIR/influxd_darwin_amd64"

        # Create archive
        archive_dir=influxdb2-${version}-darwin-amd64
        archive=${archive_dir}.tar.gz

        mkdir ${archive_dir}
        cp ${ROOT_DIR}/LICENSE ${ROOT_DIR}/README.md influxd ${archive_dir}/
        tar czf ${archive} ${archive_dir}

        # Hash the archive
        sha256sum ${archive} >> ${sha_file}

        # Copy the archive to the output dir.
        cp ${archive} ${out_dir}/
    )

    # Windows AMD64
    (
        set -eo pipefail
        cd "$DIST_DIR/influxd_windows_amd64"

        # Create archive
        archive_dir=influxdb2-${version}-windows-amd64
        archive=${archive_dir}.zip

        mkdir ${archive_dir}
        cp ${ROOT_DIR}/LICENSE ${ROOT_DIR}/README.md influxd.exe ${archive_dir}/
        zip ${archive} ${archive_dir}

        # Hash the archive
        sha256sum ${archive} >> ${sha_file}

        # Copy the archive to the output dir.
        cp ${archive} ${out_dir}/
    )
}

main ${@}