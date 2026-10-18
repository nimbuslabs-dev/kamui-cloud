from sklearn.datasets import load_digits


def load_data():
    digits = load_digits()
    return digits.data, digits.target


def main():
    X, _ = load_data()
    print(f"Loaded {X.shape[0]} samples with {X.shape[1]} features")


if __name__ == "__main__":
    main()