"""KB-1 embedding service. Trusted-boundary component: the publisher and the
gateway send text and receive L2-normalized vectors; /model reports what the
epoch must bind (model name, dimension, weights sha256). It runs as a host
process or, in a knowledge-product deployment, as the kb-embed container of
evaluation/kb-formal/compose.kb.yaml with the frozen virtualenv and model
mounted read-only (tools/kb-embed/README.md)."""
import hashlib, os, time
from typing import List
import torch
from fastapi import FastAPI
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

MODEL_DIR = os.environ.get("KB_EMBED_MODEL_DIR", "/home/wmm/kb-embed/models/bge-m3")
MODEL_NAME = os.environ.get("KB_EMBED_MODEL_NAME", "bge-m3")
MAX_BATCH = int(os.environ.get("KB_EMBED_MAX_BATCH", "256"))
INNER_BATCH = int(os.environ.get("KB_EMBED_BATCH", "16"))
PAUSE_MS = int(os.environ.get("KB_EMBED_PAUSE_MS", "0"))

def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()

WEIGHTS = os.path.join(MODEL_DIR, "pytorch_model.bin")
WEIGHTS_SHA256 = sha256_file(WEIGHTS)
device = os.environ.get("KB_EMBED_DEVICE") or ("cuda" if torch.cuda.is_available() else "cpu")
model = SentenceTransformer(MODEL_DIR, device=device)
DIMENSION = model.get_sentence_embedding_dimension()
app = FastAPI()

class EmbedRequest(BaseModel):
    texts: List[str]

@app.get("/model")
def model_info():
    return {"version": "taskgate-kb-embedding-service-v1", "model": MODEL_NAME, "dimension": DIMENSION,
            "weights_sha256": WEIGHTS_SHA256, "device": device, "normalized": True}

@app.post("/embed")
def embed(request: EmbedRequest):
    if not request.texts or len(request.texts) > MAX_BATCH:
        return {"error": f"texts must have 1..{MAX_BATCH} items"}, 400
    started = time.time()
    rows = []
    for start in range(0, len(request.texts), INNER_BATCH):
        rows.extend(model.encode(request.texts[start:start + INNER_BATCH], batch_size=INNER_BATCH, normalize_embeddings=True, convert_to_numpy=True))
        if PAUSE_MS:
            time.sleep(PAUSE_MS / 1000.0)
    vectors = rows
    return {"model": MODEL_NAME, "dimension": DIMENSION, "weights_sha256": WEIGHTS_SHA256,
            "vectors": [[round(float(x), 6) for x in row] for row in vectors], "elapsed_ms": int((time.time() - started) * 1000)}

@app.get("/health")
def health():
    return {"ok": True}
