# The Jokku website: built with Node, then served as static files by Caddy.
# On a Jokku server, deploy it from the repo root with build-dir set to
# website (see README.md).
#
# The base images are pinned by digest, so a tag moving upstream never turns
# a deploy into a cold, uncached build. To update one, run
#   docker buildx imagetools inspect node:24-alpine
# and paste the new digest.

FROM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS build
WORKDIR /app
COPY package.json package-lock.json ./
# The server's BuildKit keeps this npm cache between builds, so a changed
# lockfile only downloads the packages that changed.
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund --prefer-offline
COPY . .
RUN npm run build

FROM caddy:2-alpine@sha256:d8542f48d34a9cf4e4c11a478865229840e87e4c96ea3f439101f31a5d35f75f
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/dist/public /srv
# The newest EXPOSE is the port Jokku routes to; the Caddyfile listens on
# $PORT, which Jokku sets to it.
EXPOSE 8080
