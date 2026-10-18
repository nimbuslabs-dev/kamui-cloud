from train import load_data


def test_load_data_shapes():
    X, y = load_data()
    assert X.shape == (1797, 64)
    assert len(y) == 1797