FROM node:24-bookworm-slim AS node

FROM python:3.11-slim-bookworm

COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

COPY images/pi-gentle-python/apt-packages.txt /tmp/apt-packages.txt
RUN apt-get update \
    && xargs -r apt-get install -y --no-install-recommends < /tmp/apt-packages.txt \
    && rm -rf /var/lib/apt/lists/* /tmp/apt-packages.txt

ENV GENTLE_AI_NO_SELF_UPDATE=1 \
    GENTLE_SHELL_NO_AUTO_SETUP=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# Immutable tools stay outside HOME: the benchmark mounts a fresh agent home.
WORKDIR /opt/aqp/agent
COPY images/pi-gentle-python/package.json images/pi-gentle-python/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund \
    && node --input-type=module -e "import { installGentleAi } from './node_modules/gentle-pi/scripts/gentle-ai-installer.mjs'; import { symlinkSync } from 'node:fs'; const { binaryPath } = await installGentleAi(); symlinkSync(binaryPath, '/usr/local/bin/gentle-ai');" \
    && ln -s /opt/aqp/agent/node_modules/.bin/pi /usr/local/bin/pi \
    && ln -s /opt/aqp/agent/node_modules/.bin/gentle-shell /usr/local/bin/gentle-shell \
    && chmod -R a+rX /opt/aqp/agent \
    && npm cache clean --force

WORKDIR /app
COPY . .
RUN pip install --no-cache-dir -e '.[dev]'

RUN groupadd --gid 1000 agent \
    && useradd --uid 1000 --gid agent --create-home --home-dir /home/agent agent
ENV HOME=/home/agent
USER agent

CMD ["pytest", "-p", "no:tldr", "-v"]
