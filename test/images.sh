# Sourced by the e2e scripts. $lib is where they pull Docker Hub's official
# images from: public.ecr.aws's copy, unless it refuses this runner (some
# days it answers busy GitHub runners "429 Data limit exceeded", and the
# test app never builds), then Google's mirror.gcr.io.

# serves says whether a registry hands this runner busybox's manifest.
serves() {
	local token=
	if [ "$1" = public.ecr.aws ]; then
		token=$(curl -fsS --max-time 10 https://public.ecr.aws/token/ | sed -E 's/.*"token":"([^"]+)".*/\1/') || return 1
	fi
	[ "$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' ${token:+-H "Authorization: Bearer $token"} \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
		"https://$1/v2/$2/busybox/manifests/1.36")" = 200 ]
}

if serves public.ecr.aws docker/library; then
	lib=public.ecr.aws/docker/library
elif serves mirror.gcr.io library; then
	echo "public.ecr.aws refuses this runner: pulling images from mirror.gcr.io" >&2
	lib=mirror.gcr.io/library
else
	echo "neither public.ecr.aws nor mirror.gcr.io serves this runner; trying public.ecr.aws anyway" >&2
	lib=public.ecr.aws/docker/library
fi

# with_lib points the files' image references at $lib, for files written
# from quoted heredocs.
with_lib() { sed -i "s|public.ecr.aws/docker/library|$lib|g" "$@"; }
