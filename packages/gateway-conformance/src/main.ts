#!/usr/bin/env node
import { main } from './cli.js';

main()
  .then((code) => process.exit(code))
  .catch((err: unknown) => {
    console.error(err);
    process.exit(1);
  });
