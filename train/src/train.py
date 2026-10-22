from pathlib import Path

import joblib
from sklearn.datasets import load_digits
from sklearn.metrics import accuracy_score
from sklearn.model_selection import train_test_split
from sklearn.svm import SVC

MODEL_PATH = Path(__file__).resolve().parent.parent / "models" / "digits.joblib"


def load_data():
    digits = load_digits()
    return digits.data, digits.target


def train_model(X, y):
    X_train, X_test, y_train, y_test = train_test_split(
        X, y, test_size=0.2, random_state=42
    )
    model = SVC(gamma=0.001)
    model.fit(X_train, y_train)
    accuracy = accuracy_score(y_test, model.predict(X_test))
    return model, accuracy


def save_model(model, path=MODEL_PATH):
    path.parent.mkdir(parents=True, exist_ok=True)
    joblib.dump(model, path)
    return path


def main():
    X, y = load_data()
    model, accuracy = train_model(X, y)
    print(f"Accuracy: {accuracy:.4f}")
    path = save_model(model)
    print(f"Model saved to {path}")


if __name__ == "__main__":
    main()