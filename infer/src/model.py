import os
from pathlib import Path

import joblib

MODEL_ENV_VAR = "KAMUI_MODEL_PATH"


class ModelNotAvailableError(Exception):
    pass


def load_model():
    path = os.environ.get(MODEL_ENV_VAR)
    if not path:
        raise ModelNotAvailableError(
            f"{MODEL_ENV_VAR} is not set. Set it to the path of the trained model file."
        )
    model_file = Path(path)
    if not model_file.is_file():
        raise ModelNotAvailableError(f"Model file not found: {model_file}")
    return joblib.load(model_file)
