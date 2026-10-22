from fastapi import FastAPI
from pydantic import BaseModel, Field

app = FastAPI(title="Kamui Infer")


class PredictRequest(BaseModel):
    features: list[float] = Field(min_length=64, max_length=64)


@app.get("/healthz")
def healthz():
    return {"status": "ok"}


@app.post("/predict")
def predict(request: PredictRequest):
    return {"prediction": 0, "note": "dummy response"}
