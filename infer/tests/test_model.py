import pytest
from model import ModelNotAvailableError, load_model


def test_load_model_without_env_var(monkeypatch):
    monkeypatch.delenv("KAMUI_MODEL_PATH", raising=False)
    with pytest.raises(ModelNotAvailableError):
        load_model()


def test_load_model_missing_file(monkeypatch, tmp_path):
    monkeypatch.setenv("KAMUI_MODEL_PATH", str(tmp_path / "nope.joblib"))
    with pytest.raises(ModelNotAvailableError):
        load_model()
