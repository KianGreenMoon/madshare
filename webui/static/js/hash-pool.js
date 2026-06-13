// hash-pool.js — a small pool of hash-workers with adaptive, polite concurrency.
// hashFile(file) → Promise<sha256-hex>. Workers are spawned lazily on first use
// and torn down with terminate(). See docs/api/upload.md.

// poolSize: half the logical cores, clamped to [1, 4] (past ~4 the bottleneck is
// upload bandwidth, not hashing), backing off to 1 on low-memory devices.
function poolSize() {
  const cores = navigator.hardwareConcurrency || 4;
  let n = Math.min(4, Math.max(1, cores >> 1));
  if ((navigator.deviceMemory || 4) <= 2) n = 1;
  return n;
}

export function createHashPool(workerUrl = '/static/js/hash-worker.js') {
  const size = poolSize();
  const workers = [];
  const idle = [];
  const waiting = [];               // queued { file, resolve, reject }
  const pending = new Map();        // id → { resolve, reject }
  let nextId = 1;
  let spawned = false;

  function spawn() {
    if (spawned) return;
    spawned = true;
    for (let i = 0; i < size; i++) {
      const w = new Worker(workerUrl, { type: 'module' });
      w.onmessage = (e) => {
        const { id, hex, error } = e.data;
        const job = pending.get(id);
        if (!job) return;
        pending.delete(id);
        idle.push(w);
        if (error) job.reject(new Error(error)); else job.resolve(hex);
        dispatch();
      };
      w.onerror = () => {
        // A worker errored (e.g. module workers unsupported). Reject in-flight and
        // queued jobs so callers fall back to a plain upload instead of hanging.
        for (const [, job] of pending) job.reject(new Error('hash worker failed'));
        pending.clear();
        for (const job of waiting) job.reject(new Error('hash worker failed'));
        waiting.length = 0;
      };
      workers.push(w);
      idle.push(w);
    }
  }

  function dispatch() {
    while (idle.length && waiting.length) {
      const w = idle.pop();
      const job = waiting.shift();
      const id = nextId++;
      pending.set(id, { resolve: job.resolve, reject: job.reject });
      w.postMessage({ id, file: job.file });
    }
  }

  return {
    size,
    hashFile(file) {
      return new Promise((resolve, reject) => {
        try { spawn(); } catch (err) { reject(err); return; } // module workers unsupported
        waiting.push({ file, resolve, reject });
        dispatch();
      });
    },
    terminate() {
      for (const w of workers) { try { w.terminate(); } catch { /* ignore */ } }
      workers.length = 0;
      idle.length = 0;
      spawned = false;
      for (const job of waiting) job.reject(new Error('aborted'));
      waiting.length = 0;
      for (const [, job] of pending) job.reject(new Error('aborted'));
      pending.clear();
    },
  };
}
