// Shared setup()/teardown() re-exported by every scenario. setup() runs once:
// mint a bearer token per role, then discover live library content. teardown()
// revokes the tokens. The returned object is passed to every VU iteration.

import { mintToken, revokeToken } from './auth.js';
import { discover } from './discover.js';

export function setup() {
  const minted = {
    user: mintToken('user'),
    uploader: mintToken('uploader'),
    admin: mintToken('admin'),
  };
  const tokens = {
    user: minted.user.token,
    uploader: minted.uploader.token,
    admin: minted.admin.token,
  };
  const tokenIds = {
    user: minted.user.id,
    uploader: minted.uploader.id,
    admin: minted.admin.id,
  };

  const discovered = discover(tokens);

  return { tokens, tokenIds, ...discovered };
}

export function teardown(data) {
  if (!data || !data.tokens) return;
  for (const role of ['user', 'uploader', 'admin']) {
    revokeToken(data.tokenIds[role], data.tokens[role]);
  }
}
