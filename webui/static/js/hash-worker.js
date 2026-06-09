// hash-worker.js — a module worker that hashes a File off the main thread so the
// UI stays responsive during a big upload batch. Driven by hash-pool.js.
import { hashBlob } from './sha256.js';

self.onmessage = async (e) => {
  const { id, file } = e.data;
  try {
    const hex = await hashBlob(file);
    self.postMessage({ id, hex });
  } catch (err) {
    self.postMessage({ id, error: String((err && err.message) || err) });
  }
};
