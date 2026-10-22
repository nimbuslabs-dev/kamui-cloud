from fastapi import FastAPI

app = FastAPI(title="Kamui Infer")


@app.get("/healthz")
def healthz():
    return {"status": "ok"}


@app.post("/predict")
def predict():
    return {"prediction": 0, "note": "dummy response"}
