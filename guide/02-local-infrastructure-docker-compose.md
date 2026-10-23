# Chapter 2: Local Infrastructure with Docker Compose

Welcome to Chapter 2. In this chapter, we step away from Go code for a moment to build the bedrock of our local development environment.

A distributed system cannot function without its backing services. Rather than installing PostgreSQL, NATS JetStream, and MinIO directly on your Arch Linux host—polluting your system with background daemons, conflicting ports, and lingering state—we will define our entire backing infrastructure as code using **Docker Compose**.

By the end of this chapter, you will be able to spin up, inspect, and tear down our complete infrastructure cluster with single, reproducible commands.

---

## 1. Objectives & Overview

In this chapter, you will:
- Understand container networking and bridge networks.
- Learn why persistent named volumes are mandatory for databases, message streams, and object storage.
- Configure PostgreSQL 16, NATS 2.10 (with JetStream enabled), and MinIO with isolated ports and credentials.
- Implement container healthchecks so services only accept traffic when fully initialized.
- Write a clean `Makefile` to automate infrastructure lifecycle commands on Arch Linux.
- Verify connectivity to all three services using CLI tools and web consoles.

---

## 2. Conceptual Deep Dive (The "Why")

### Container Networking: The Bridge Network
When containers run in isolation, they cannot communicate with each other unless they attach to a shared network.

Docker Compose automatically creates a user-defined **bridge network** for our project. 

```
+-----------------------------------------------------------------+
| Docker Bridge Network ("docpub-net")                            |
|                                                                 |
|  +------------------+  +--------------------+  +--------------+ |
|  |    PostgreSQL    |  |   NATS JetStream   |  |    MinIO     | |
|  |  (dns: postgres) |  |    (dns: nats)     |  | (dns: minio) | |
|  |    port: 5432    |  |     port: 4222     |  |  port: 9000  | |
|  +--------+---------+  +---------+----------+  +-------+------+ |
|           |                      |                     |        |
+-----------|----------------------|---------------------|--------+
            |                      |                     |
       Host Bind              Host Binds            Host Binds
       127.0.0.1:5432         127.0.0.1:4222 (Client) 127.0.0.1:9000 (API)
                              127.0.0.1:8222 (Monitor) 127.0.0.1:9001 (Console)
```

Inside this network, Docker runs an embedded DNS server. This means:
- The API service will connect to PostgreSQL using hostname `postgres:5432` (inside Docker) or `localhost:5432` (when running locally on your Arch host).
- Services do not need hardcoded container IP addresses.

### Why Persistent Named Volumes?
Containers are ephemeral by default: when a container stops and is deleted, any data written to its filesystem is destroyed.

To preserve database records, uploaded files, and queued messages across restarts, we attach **named volumes**:
- `pgdata` -> mounted to `/var/lib/postgresql/data`
- `miniodata` -> mounted to `/data`
- `natsdata` -> mounted to `/data`

Named volumes are managed by Docker on Arch Linux under `/var/lib/docker/volumes/`. They persist even when containers are destroyed via `docker compose down`.

### Why Native Healthchecks Matter
Starting a container is not instantaneous:
1. Docker starts the container process.
2. The service initializes shared memory, reads configuration files, and runs recovery checks.
3. Only after several seconds does it begin accepting TCP connections.

If an application attempts to connect immediately after container launch, it crashes with `connection refused`. We configure native `healthcheck` commands in Docker Compose so dependent services know the precise moment a service is ready for traffic.

---

## 3. Step-by-Step Implementation

We will create our Compose specification in `deploy/docker/docker-compose.yml`. To prevent cognitive overload, we will review it in small, logical chunks.

### Step 3.1: PostgreSQL Service Definition
First, we define PostgreSQL with an automated `pg_isready` healthcheck.

```yaml
services:
  postgres:
    image: postgres:16-alpine
    container_name: docpub-postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: docpub
      POSTGRES_PASSWORD: docpub_secret
      POSTGRES_DB: docpub
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U docpub -d docpub"]
      interval: 5s
      timeout: 5s
      retries: 5
```

**Line-by-line explanation:**
- `image: postgres:16-alpine`: We use Alpine Linux-based images to keep download size and security attack surface minimal.
- `container_name: docpub-postgres`: A predictable name for log inspection and debugging.
- `environment`: Sets the initial superuser credentials and default database name.
- `ports: "5432:5432"`: Maps port 5432 inside the container to port 5432 on your Arch Linux host.
- `volumes: pgdata:...`: Directs PostgreSQL to write all tables and write-ahead logs to our persistent volume.
- `healthcheck`: Runs `pg_isready -U docpub -d docpub` every 5 seconds. Docker marks the container `healthy` only when PostgreSQL responds.

---

### Step 3.2: NATS JetStream Service Definition
Next, we add NATS with JetStream enabled for fast, lightweight event streaming.

```yaml
  nats:
    image: nats:2.10-alpine
    container_name: docpub-nats
    restart: unless-stopped
    command: ["-js", "-sd", "/data", "-m", "8222"]
    ports:
      - "4222:4222"  # Client connection port
      - "8222:8222"  # HTTP monitoring & health port
    volumes:
      - natsdata:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8222/healthz"]
      interval: 5s
      timeout: 3s
      retries: 5
```

**Line-by-line explanation:**
- `image: nats:2.10-alpine`: Lightweight, single Go binary container image (<20MB total footprint).
- `command: ["-js", "-sd", "/data", "-m", "8222"]`:
  - `-js`: Enables JetStream persistence engine.
  - `-sd /data`: Sets the storage directory for persistent message streams to `/data`.
  - `-m 8222`: Enables HTTP monitoring and metrics on port 8222.
- `ports: "4222:4222"`: Standard NATS client protocol port.
- `ports: "8222:8222"`: HTTP monitoring port returning JSON statistics and health checks.
- `healthcheck`: Uses Alpine's built-in `wget --spider` to ping `http://localhost:8222/healthz`.

---

### Step 3.3: MinIO Service Definition
Now we configure MinIO, our high-performance S3-compatible object store.

```yaml
  minio:
    image: cgr.dev/chainguard/minio:latest
    container_name: docpub-minio
    restart: unless-stopped
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    ports:
      - "9000:9000"  # S3 standard API
      - "9001:9001"  # MinIO browser console
    volumes:
      - miniodata:/data
    healthcheck:
      test: ["CMD", "mc", "ready", "local"]
      interval: 5s
      timeout: 5s
      retries: 5
```

**Line-by-line explanation:**
- `command: server /data --console-address ":9001"`: Launches the MinIO storage server pointing to `/data` and binds the web console to port 9001.
- `ports: "9000:9000"`: The S3 API endpoint our Go `minio-go` client communicates with.
- `ports: "9001:9001"`: The visual storage console accessible in your browser at `http://localhost:9001`.

---

### Step 3.4: Complete Compose File & Volumes
Here is the assembled file saved at `deploy/docker/docker-compose.yml`:

```yaml
version: "3.8"

services:
  postgres:
    image: postgres:16-alpine
    container_name: docpub-postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: docpub
      POSTGRES_PASSWORD: docpub_secret
      POSTGRES_DB: docpub
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U docpub -d docpub"]
      interval: 5s
      timeout: 5s
      retries: 5

  nats:
    image: nats:2.10-alpine
    container_name: docpub-nats
    restart: unless-stopped
    command: ["-js", "-sd", "/data", "-m", "8222"]
    ports:
      - "4222:4222"
      - "8222:8222"
    volumes:
      - natsdata:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8222/healthz"]
      interval: 5s
      timeout: 3s
      retries: 5

  minio:
    image: cgr.dev/chainguard/minio:latest
    container_name: docpub-minio
    restart: unless-stopped
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    ports:
      - "9000:9000"
      - "9001:9001"
    volumes:
      - miniodata:/data
    healthcheck:
      test: ["CMD", "mc", "ready", "local"]
      interval: 5s
      timeout: 5s
      retries: 5

volumes:
  pgdata:
    name: docpub-pgdata
  natsdata:
    name: docpub-natsdata
  miniodata:
    name: docpub-miniodata
```

---

### Step 3.5: Automating with a Root Makefile
To avoid memorizing long Compose commands and flags, we create a `Makefile` at the repository root.

```makefile
.PHONY: help up down logs ps clean

help:
	@echo "Available commands:"
	@echo "  make up     - Start all infrastructure containers in background"
	@echo "  make down   - Stop and remove infrastructure containers"
	@echo "  make logs   - Tail logs from all containers"
	@echo "  make ps     - Check status and health of containers"
	@echo "  make clean  - Stop containers and remove all persistent volumes (DATA LOSS)"

up:
	docker compose -f deploy/docker/docker-compose.yml up -d

down:
	docker compose -f deploy/docker/docker-compose.yml down

logs:
	docker compose -f deploy/docker/docker-compose.yml logs -f

ps:
	docker compose -f deploy/docker/docker-compose.yml ps

clean:
	docker compose -f deploy/docker/docker-compose.yml down -v
```

---

## 4. Terminal Commands & Infrastructure Execution

Let's execute the setup on your Arch Linux system.

### Step 4.1: Ensure Docker Daemon is Active
Ensure systemd has started the Docker service:

```bash
sudo systemctl status docker
```

If it is inactive, start it:

```bash
sudo systemctl enable --now docker
```

### Step 4.2: Start the Infrastructure
From the repository root, run:

```bash
make up
```

**Flag explanation:**
- `-f deploy/docker/docker-compose.yml`: Tells docker-compose where the configuration file lives.
- `up`: Builds, creates, and starts containers.
- `-d`: **Detached mode**. Runs containers in the background, freeing your terminal.

**Sample terminal output:**
```
[+] Running 6/6
 ✔ Volume "docpub-miniodata"      Created
 ✔ Volume "docpub-natsdata"       Created
 ✔ Volume "docpub-pgdata"         Created
 ✔ Container docpub-minio         Started
 ✔ Container docpub-postgres      Started
 ✔ Container docpub-nats          Started
```

---

## 5. Verification Playbook

Never assume infrastructure is running—always verify with concrete checks.

### Check 1: Inspect Container Health
Run `make ps` to inspect container states:

```bash
make ps
```

**Expected terminal output (notice the `(healthy)` indicator):**
```
NAME              IMAGE                              STATUS                   PORTS
docpub-minio      cgr.dev/chainguard/minio:latest    Up 15s (healthy)         0.0.0.0:9000->9000/tcp, 0.0.0.0:9001->9001/tcp
docpub-postgres   postgres:16-alpine                 Up 15s (healthy)         0.0.0.0:5432->5432/tcp
docpub-nats       nats:2.10-alpine                   Up 15s (healthy)         0.0.0.0:4222->4222/tcp, 0.0.0.0:8222->8222/tcp
```

---

### Check 2: Verify PostgreSQL Connectivity
Query PostgreSQL directly using `docker exec`:

```bash
docker exec -it docpub-postgres psql -U docpub -d docpub -c "SELECT version();"
```

**Expected output:**
```
                                                 version                                                 
---------------------------------------------------------------------------------------------------------
 PostgreSQL 16.x on x86_64-pc-linux-musl, compiled by gcc...
(1 row)
```

---

### Check 3: Verify MinIO Console Access
Open your browser or run curl against the MinIO health endpoint:

```bash
curl -i http://localhost:9000/minio/health/live
```

**Expected output:**
```http
HTTP/1.1 200 OK
Server: MinIO
...
```
You can also open **http://localhost:9001** in your browser and log in with username `minioadmin` and password `minioadmin`.

---

### Check 4: Verify NATS JetStream Health & Monitoring
Test the NATS HTTP monitoring API:

```bash
curl -i http://localhost:8222/healthz
```

**Expected output:**
```http
HTTP/1.1 200 OK
Content-Type: application/json
Date: Fri, 23 Oct 2026 17:00:00 GMT
Content-Length: 17

{"status":"ok"}
```

Query the JetStream statistics endpoint:
```bash
curl -s http://localhost:8222/jsz | grep -o '"jetstream":[^,]*'
```

**Expected output:**
```json
"jetstream":true
```

---

## 6. Common Gotchas & Troubleshooting

1. **Port Already in Use (`bind: address already in use`):**
   - *Symptom*: Error response from daemon: `driver failed programming external connectivity on endpoint docpub-postgres: bind: address already in use`.
   - *Cause*: A local PostgreSQL or NATS daemon is already running on your Arch host.
   - *Fix*: Check which process owns the port using Arch Linux `ss`:
     ```bash
     sudo ss -tulpn | grep -E ':(5432|4222|8222|9000)'
     ```
     Stop conflicting host service:
     ```bash
     sudo systemctl stop postgresql
     ```

2. **Docker Socket Permission Denied:**
   - *Symptom*: `permission denied while trying to connect to the Docker daemon socket`.
   - *Cause*: Your user account is not in the `docker` group.
   - *Fix*:
     ```bash
     sudo usermod -aG docker $USER
     newgrp docker
     ```

3. **Resetting Corrupt State:**
   - *Symptom*: NATS or PostgreSQL fails to boot due to dirty volume state.
   - *Fix*: Run `make clean` to delete all containers and volumes, then `make up` to start fresh.

---

## 7. Summary & Next Steps

### Key Takeaways
- Backing services run cleanly isolated in Docker without polluting your Arch Linux environment.
- NATS JetStream runs as a fast, single Go binary requiring ~20MB RAM and zero external runtimes.
- Persistent named volumes preserve data across container restarts.
- Healthchecks provide deterministic feedback on whether services are ready to accept connections.
- The `Makefile` simplifies operational commands down to `make up`, `make down`, and `make ps`.

### Self-Check Questions Before Advancing
1. What is the difference between connecting to PostgreSQL on `localhost:5432` vs `postgres:5432`?
2. Why should we use `docker compose down` instead of `docker compose down -v` during normal development?
3. What is the purpose of the `-js` flag in the NATS container command?

---

In **[Chapter 3: Database Migrations and Connection Pooling](03-database-migrations-and-pooling.md)**, we will connect Go to our PostgreSQL database, write versioned SQL migrations using `golang-migrate`, and configure a resilient `pgxpool.Pool` with proper connection limits and timeouts.
