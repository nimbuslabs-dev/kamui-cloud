from app import app
from fastapi.testclient import TestClient

client = TestClient(app)


def test_healthz():
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_predict_dummy():
    response = client.post("/predict")
    assert response.status_code == 200
    assert "prediction" in response.json()
