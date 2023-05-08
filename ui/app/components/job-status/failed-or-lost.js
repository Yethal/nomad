/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class JobStatusFailedOrLostComponent extends Component {
  get shouldLinkToAllocations() {
    return this.args.title !== 'Restarted' && this.args.allocs.length;
  }
}
