from app import app
from fastapi.testclient import TestClient

client = TestClient(app)


def test_healthz():
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_predict_valid_input():
    response = client.post("/predict", json={"features": [0.0] * 64})
    assert response.status_code == 200
    assert "prediction" in response.json()


def test_predict_too_few_features():
    response = client.post("/predict", json={"features": [0.0] * 10})
    assert response.status_code == 422


def test_predict_not_numbers():
    response = client.post("/predict", json={"features": ["a"] * 64})
    assert response.status_code == 422


def test_predict_missing_features():
    response = client.post("/predict", json={})
    assert response.status_code == 422
