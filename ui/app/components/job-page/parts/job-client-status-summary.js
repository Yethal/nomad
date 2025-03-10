/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@ember/component';
import { action, computed } from '@ember/object';
import { inject as service } from '@ember/service';
import { camelize } from '@ember/string';
import { classNames } from '@ember-decorators/component';
import classic from 'ember-classic-decorator';
import jobClientStatus from 'nomad-ui/utils/properties/job-client-status';

@classic
@classNames('boxed-section')
export default class JobClientStatusSummary extends Component {
  @service router;
  @service store;

  @jobClientStatus('nodes', 'job') jobClientStatus;

  get nodes() {
    return this.store.peekAll('node');
  }

  job = null;

  @action
  gotoClients(statusFilter) {
    this.router.transitionTo('jobs.job.clients', this.job, {
      queryParams: {
        status: JSON.stringify(statusFilter),
      },
    });
  }

  @computed
  get isExpanded() {
    const storageValue = window.localStorage.nomadExpandJobClientStatusSummary;
    return storageValue != null ? JSON.parse(storageValue) : true;
  }

  @action
  onSliceClick(ev, slice) {
    this.gotoClients([camelize(slice.className)]);
  }

  persist(item, isOpen) {
    window.localStorage.nomadExpandJobClientStatusSummary = isOpen;
    this.notifyPropertyChange('isExpanded');
  }
}
