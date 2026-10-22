import joblib

from train import load_data, save_model, train_model


def test_load_data_shapes():
    X, y = load_data()
    assert X.shape == (1797, 64)
    assert len(y) == 1797


def test_train_and_save(tmp_path):
    X, y = load_data()
    model, accuracy = train_model(X, y)
    assert accuracy > 0.9
    path = save_model(model, tmp_path / "digits.joblib")
    assert path.exists()
    assert joblib.load(path).predict(X[:5]).shape == (5,)