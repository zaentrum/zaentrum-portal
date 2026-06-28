# build the SPA (vendored @nalet/design-system tarball is in ./vendor)
FROM registry.nalet.cloud/infrastructure/library/node:20-alpine AS build
WORKDIR /src
COPY package.json package-lock.json* ./
COPY vendor ./vendor
RUN if [ -f package-lock.json ]; then npm ci --no-audit --no-fund; else npm install --no-audit --no-fund; fi
COPY . .
RUN npm run build

# serve under /portal (Vite base); bare host 302s to the launchpad
FROM registry.nalet.cloud/infrastructure/library/nginxinc/nginx-unprivileged:1.27-alpine
COPY --from=build /src/dist /usr/share/nginx/html/portal
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 8080
LABEL org.opencontainers.image.source="https://gitlab.nalet.cloud/stube/zaentrum-portal"
LABEL org.opencontainers.image.title="zaentrum-portal"
