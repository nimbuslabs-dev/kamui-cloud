import nextVitals from 'eslint-config-next/core-web-vitals';

export default [
  ...nextVitals,
  {
    rules: {
      '@next/next/no-img-element': 'off',
      'react/no-unescaped-entities': 'off',
    },
  },
];
